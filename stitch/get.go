package stitch

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/tools/go/vcs"
	"os"
	"path/filepath"
	"text/scanner"

	"github.com/NetSys/quilt/util"

	log "github.com/Sirupsen/logrus"
	homedir "github.com/mitchellh/go-homedir"

	"github.com/spf13/afero"
)

// QuiltPathKey is the environment variable key we use to lookup the Quilt path.
const QuiltPathKey = "QUILT_PATH"

// GetQuiltPath returns the user-defined QUILT_PATH, or the default absolute QUILT_PATH,
// which is ~/.quilt if the user did not specify a QUILT_PATH.
func GetQuiltPath() string {
	if quiltPath := os.Getenv(QuiltPathKey); quiltPath != "" {
		return quiltPath
	}

	dir, err := homedir.Dir()
	if err != nil {
		log.WithError(err).Fatalf("Failed to get user's homedir for "+
			"%s generation", QuiltPathKey)
	}

	return filepath.Join(dir, ".quilt")
}

// ImportGetter provides functions for working with imports.
type ImportGetter struct {
	Path         string
	AutoDownload bool

	repoFactory func(repo string) (repo, error)
}

func (getter ImportGetter) withAutoDownload(autoDownload bool) ImportGetter {
	return ImportGetter{
		Path:         getter.Path,
		AutoDownload: autoDownload,
		repoFactory:  getter.repoFactory,
	}
}

type repo interface {
	// Pull the latest changes in the repo to `dir`.
	update(dir string) error

	// Checkout the repo to `dir`.
	create(dir string) error

	// Get the root of the repo.
	root() string
}

// `goRepo` is a wrapper around `vcs.RepoRoot` that satisfies the `repo` interface.
type goRepo struct {
	repo *vcs.RepoRoot
}

func (gr goRepo) update(dir string) error {
	return gr.repo.VCS.Download(dir)
}

func (gr goRepo) create(dir string) error {
	return gr.repo.VCS.Create(dir, gr.repo.Repo)
}

func (gr goRepo) root() string {
	return gr.repo.Root
}

func goRepoFactory(repoName string) (repo, error) {
	vcsRepo, err := vcs.RepoRootForImportPath(repoName, true)
	return goRepo{vcsRepo}, err
}

// DefaultImportGetter uses the default QUILT_PATH, and doesn't automatically
// download imports.
var DefaultImportGetter = ImportGetter{
	Path:        GetQuiltPath(),
	repoFactory: goRepoFactory,
}

// Get takes in an import path `repoName`, and attempts to download the
// repository associated with that repoName.
func (getter ImportGetter) Get(repoName string) error {
	path, err := getter.downloadSpec(repoName)
	if err != nil {
		return err
	}
	return getter.resolveSpecImports(path)
}

func (getter ImportGetter) downloadSpec(repoName string) (string, error) {
	repo, err := getter.repoFactory(repoName)
	if err != nil {
		return "", err
	}

	path := filepath.Join(getter.Path, repo.root())
	if _, statErr := util.AppFs.Stat(path); os.IsNotExist(statErr) {
		log.Info(fmt.Sprintf("Cloning %s into %s", repo.root(), path))
		err = repo.create(path)
	} else {
		log.Info(fmt.Sprintf("Updating %s in %s", repo.root(), path))
		err = repo.update(path)
	}
	return path, err
}

func (getter ImportGetter) resolveSpecImports(folder string) error {
	return afero.Walk(util.AppFs, folder, getter.checkSpec)
}

func (getter ImportGetter) checkSpec(file string, _ os.FileInfo, _ error) error {
	if filepath.Ext(file) != ".spec" {
		return nil
	}
	f, err := util.Open(file)

	if err != nil {
		return err
	}
	defer f.Close()

	sc := scanner.Scanner{
		Position: scanner.Position{
			Filename: file,
		},
	}
	_, err = Compile(*sc.Init(bufio.NewReader(f)), getter.withAutoDownload(true))
	return err
}

func (getter ImportGetter) specContents(name string) (scanner.Scanner, error) {
	modulePath := filepath.Join(getter.Path, name+".spec")
	if _, err := util.AppFs.Stat(modulePath); os.IsNotExist(err) &&
		getter.AutoDownload {
		getter.Get(name)
	}

	var sc scanner.Scanner
	f, err := util.Open(modulePath)
	if err != nil {
		return sc, fmt.Errorf("unable to open import %s (path=%s)",
			name, modulePath)
	}
	sc.Filename = modulePath
	sc.Init(bufio.NewReader(f))
	return sc, nil
}

func (getter ImportGetter) resolveImports(asts []ast) ([]ast, error) {
	return getter.resolveImportsRec(asts, nil)
}

func (getter ImportGetter) resolveImportsRec(
	asts []ast, imported []string) ([]ast, error) {

	var newAsts []ast
	top := true // Imports are required to be at the top of the file.

	for _, ast := range asts {
		name := parseImport(ast)
		if name == "" {
			newAsts = append(newAsts, ast)
			top = false
			continue
		}

		if !top {
			return nil, errors.New(
				"import must be at the beginning of the module")
		}

		// Check for any import cycles.
		for _, importedModule := range imported {
			if name == importedModule {
				return nil, fmt.Errorf("import cycle: %s",
					append(imported, name))
			}
		}

		moduleScanner, err := getter.specContents(name)
		if err != nil {
			return nil, err
		}

		parsed, err := parse(moduleScanner)
		if err != nil {
			return nil, err
		}

		// Rename module name to last name in import path
		name = filepath.Base(name)
		parsed, err = getter.resolveImportsRec(parsed, append(imported, name))
		if err != nil {
			return nil, err
		}

		module := astModule{body: parsed, moduleName: astString(name)}
		newAsts = append(newAsts, module)
	}

	return newAsts, nil
}

func parseImport(ast ast) string {
	sexp, ok := ast.(astSexp)
	if !ok {
		return ""
	}

	if len(sexp.sexp) < 1 {
		return ""
	}

	fnName, ok := sexp.sexp[0].(astBuiltIn)
	if !ok {
		return ""
	}

	if fnName != "import" {
		return ""
	}

	name, ok := sexp.sexp[1].(astString)
	if !ok {
		return ""
	}

	return string(name)
}
