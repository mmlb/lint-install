package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/karrick/godirwalk"
	"k8s.io/klog/v2"
)

var (
	dryRunFlag     = flag.Bool("dry-run", false, "Display changes to make")
	goFlag         = flag.String("go", "error", "Level to lint Go with: [ignore, warn, error]")
	shellFlag      = flag.String("shell", "error", "Level to lint Shell with: [ignore, warn, error]")
	dockerfileFlag = flag.String("dockerfile", "error", "Level to lint Dockerfile with: [ignore, warn, error]")
	makeFileName   = flag.String("makefile", "Makefile", "name of Makefile to update")

	//go:embed .golangci.yml
	goLintConfig []byte

	//go:embed Makefile.tmpl
	makeTmpl string
)

type Language int

const (
	Go Language = iota
	Shell
	Dockerfile
)

type Config struct {
	Makefile     string
	Args         string
	Go           string
	Dockerfile   string
	Shell        string
	LintCommands []string
	FixCommands  []string
}

// applicableLinters returns a list of languages with known linters within a given directory.
func applicableLinters(root string) (map[Language]bool, error) {
	klog.Infof("Searching for linters to use for %s ...", root)
	found := map[Language]bool{}

	err := godirwalk.Walk(root, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			if strings.HasSuffix(path, ".go") {
				found[Go] = true
			}
			if strings.HasSuffix(path, "Dockerfile") {
				found[Dockerfile] = true
			}
			if strings.HasSuffix(path, ".sh") {
				found[Shell] = true
			}
			return nil
		},
		Unsorted: true,
	})

	return found, err
}

// updateMakefile updates the Makefile within a project with lint rules.
func updateMakefile(root string, cfg Config, dryRun bool) (string, error) {
	dest := filepath.Join(root, cfg.Makefile)
	var existing []byte
	var err error

	if _, err = os.Stat(dest); err == nil {
		klog.Infof("Found existing %s", dest)
		existing, err = os.ReadFile(dest)
		if err != nil {
			return "", err
		}
	}

	var newRules bytes.Buffer
	t := template.Must(template.New("Makefile").Parse(makeTmpl))
	if err = t.Execute(&newRules, cfg); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}

	ignore := false
	inserted := false
	proposed := []byte{}
	for x, line := range bytes.Split(existing, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("# BEGIN: lint-install")) {
			ignore = true
			inserted = true
			proposed = append(proposed, newRules.Bytes()...)
			continue
		}

		if bytes.HasPrefix(line, []byte("# END: lint-install")) {
			ignore = false
			continue
		}

		if ignore {
			continue
		}

		if x > 0 {
			proposed = append(proposed, []byte("\n")...)
		}
		proposed = append(proposed, line...)
	}

	if !inserted {
		proposed = append(proposed, newRules.Bytes()...)
	}

	// trim any accidental trailing newlines
	proposed = bytes.TrimRight(proposed, "\n")
	proposed = append(proposed, byte('\n'))

	edits := myers.ComputeEdits("Makefile", string(existing), string(proposed))
	change := gotextdiff.ToUnified(filepath.Base(dest), filepath.Base(dest), string(existing), edits)
	if !dryRun {
		if err := os.WriteFile(dest, proposed, 0o600); err != nil {
			return "", err
		}
	}
	return fmt.Sprint(change), nil
}

// updateGoLint updates the golangci-lint configuration for a project.
func updateGoLint(root string, dryRun bool) (string, error) {
	dest := filepath.Join(root, ".golangci.yml")
	var existing []byte
	var err error

	if _, err = os.Stat(dest); err == nil {
		klog.Infof("Found existing %s", dest)
		existing, err = os.ReadFile(dest)
		if err != nil {
			return "", err
		}
	}

	proposed := string(goLintConfig)
	edits := myers.ComputeEdits(".golangci.yml", string(existing), proposed)
	change := gotextdiff.ToUnified(filepath.Base(dest), filepath.Base(dest), string(existing), edits)

	if !dryRun {
		if err := os.WriteFile(dest, goLintConfig, 0o600); err != nil {
			return "", err
		}
	}

	return fmt.Sprint(change), nil
}

// goLintCmd returns the appropriate golangci-lint command to run for a project.
func goLintCmd(root string, level string, fix bool) string {
	klog.Infof("Searching for go modules within %s ...", root)
	found := []string{}

	err := godirwalk.Walk(root, &godirwalk.Options{
		Callback: func(path string, de *godirwalk.Dirent) error {
			if strings.HasSuffix(path, "go.mod") {
				found = append(found, filepath.Dir(path))
			}
			return nil
		},
		Unsorted: true,
	})
	if err != nil {
		klog.Errorf("unable to find go.mod files: %v", err)
	}

	suffix := ""
	if fix {
		suffix = " --fix"
	} else if level == "warn" {
		suffix = " || true"
	}

	klog.Infof("found %d modules within %s: %s", len(found), root, found)
	if len(found) == 0 || (len(found) == 1 && found[0] == strings.Trim(root, "/")) {
		return fmt.Sprintf("out/linters/golangci-lint-$(GOLINT_VERSION)-$(LINT_ARCH) run%s", suffix)
	}

	return fmt.Sprintf(`find . -name go.mod -execdir "$(LINT_ROOT)/out/linters/golangci-lint-$(GOLINT_VERSION)-$(LINT_ARCH)" run -c "$(GOLINT_CONFIG)"%s \;`, suffix)
}

// shellLintCmd returns the appropriate shell lint command for a project.
func shellLintCmd(_ string, level string, fix bool) string {
	suffix := ""

	if fix {
		// patch(1) doesn't support patching from stdin on all platforms, so we use git apply instead
		suffix = " -f diff | git apply -p2 -"
	} else if level == "warn" {
		suffix = " || true"
	}

	return fmt.Sprintf(`out/linters/shellcheck-$(SHELLCHECK_VERSION)-$(LINT_ARCH)/shellcheck $(shell find . -name "*.sh")%s`, suffix)
}

// dockerLintCmd returns the appropriate docker lint command for a project.
func dockerLintCmd(_ string, level string) string {
	f := ""
	if level == "warn" {
		f = " --no-fail"
	}

	return fmt.Sprintf(`out/linters/hadolint-$(HADOLINT_VERSION)-$(LINT_ARCH)%s $(shell find . -name "*Dockerfile")`, f)
}

// main creates peanut butter & jelly sandwiches with utmost precision.
func main() {
	klog.InitFlags(nil)
	flag.Parse()

	if len(flag.Args()) == 0 {
		klog.Exitf("usage: lint-install [directory..]")
	}

	for _, root := range flag.Args() {
		needs, err := applicableLinters(root)
		if err != nil {
			klog.Exitf("failed to find linters: %v", err)
		}
		if len(needs) == 0 {
			continue
		}

		if needs[Go] {
			diff, err := updateGoLint(root, *dryRunFlag)
			if err != nil {
				klog.Exitf("update go lint config failed: %v", err)
			}
			if diff != "" {
				klog.Infof("go lint config changes:\n%s", diff)
			} else {
				klog.Infof("go lint config has no changes")
			}
		}

		cfg := Config{
			Args:     strings.Join(os.Args[1:], " "),
			Makefile: *makeFileName,
		}

		if needs[Go] {
			cfg.Go = *goFlag
			cfg.LintCommands = append(cfg.LintCommands, goLintCmd(root, cfg.Go, false))
			cfg.FixCommands = append(cfg.FixCommands, goLintCmd(root, cfg.Go, true))
		}
		if needs[Dockerfile] {
			cfg.Dockerfile = *dockerfileFlag
			cfg.LintCommands = append(cfg.LintCommands, dockerLintCmd(root, cfg.Dockerfile))
		}
		if needs[Shell] {
			cfg.Shell = *shellFlag
			cfg.LintCommands = append(cfg.LintCommands, shellLintCmd(root, cfg.Shell, false))
			cfg.FixCommands = append(cfg.FixCommands, shellLintCmd(root, cfg.Shell, true))
		}

		diff, err := updateMakefile(root, cfg, *dryRunFlag)
		if err != nil {
			klog.Exitf("update Makefile failed: %v", err)
		}
		if diff != "" {
			klog.Infof("Makefile changes:\n%s", diff)
		} else {
			klog.Infof("Makefile has no changes")
		}
	}
}
