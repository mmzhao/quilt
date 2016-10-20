package stitch

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/NetSys/quilt/util"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
)

func TestGetQuiltPath(t *testing.T) {
	os.Setenv(QuiltPathKey, "")
	actual := GetQuiltPath()
	usr, err := user.Current()
	if err != nil {
		t.Error(err)
	}
	expected := filepath.Join(usr.HomeDir, ".quilt")
	if actual != expected {
		t.Errorf("expected %s \n but got %s", expected, actual)
	}
}

// repoLogger logs the directories interacted with for each repo
type repoLogger struct {
	created map[string][]string
	updated map[string][]string
}

func newRepoLogger() repoLogger {
	return repoLogger{
		created: make(map[string][]string),
		updated: make(map[string][]string),
	}
}

type mockRepo struct {
	repoName string
	toCreate []file

	logger *repoLogger
}

type file struct {
	name     string
	contents string
}

func (rl *repoLogger) newRepoFactory(toCreate map[string][]file) func(string) (
	repo, error) {

	return func(repoName string) (repo, error) {
		repo := mockRepo{
			repoName: repoName,
			logger:   rl,
		}
		if toCreate != nil {
			repo.toCreate = toCreate[repoName]
		}
		return &repo, nil
	}
}

func (mr *mockRepo) update(dir string) error {
	mr.logger.updated[mr.repoName] = append(mr.logger.updated[mr.repoName], dir)
	return nil
}

func (mr *mockRepo) create(dir string) error {
	for _, toCreate := range mr.toCreate {
		util.WriteFile(
			filepath.Join(dir, toCreate.name),
			[]byte(toCreate.contents),
			0644)
	}
	mr.logger.created[mr.repoName] = append(mr.logger.created[mr.repoName], dir)
	return nil
}

// The root is always the directory.
// e.g. github.com/NetSys/quilt/specs/spark => github.com/NetSys/quilt/specs,
// NOT github.com/NetSys/quilt
func (mr *mockRepo) root() string {
	dir, _ := filepath.Split(mr.repoName)
	return dir
}

func TestImportExists(t *testing.T) {
	util.AppFs = afero.NewMemMapFs()

	util.WriteFile("test.spec", []byte(`(import "existingImport")`), 0644)
	util.WriteFile("existingImport.spec", []byte("(define dummy 1)"), 0644)

	logger := newRepoLogger()
	getter := ImportGetter{
		Path:        ".",
		repoFactory: logger.newRepoFactory(nil),
	}
	if err := getter.checkSpec("test.spec", nil, nil); err != nil {
		t.Error(err)
		return
	}

	assert.Empty(t, logger.created, "Shouldn't create any repos")
	assert.Empty(t, logger.updated, "Shouldn't update any repos")
}

func TestAutoDownload(t *testing.T) {
	util.AppFs = afero.NewMemMapFs()

	repoName := "autodownload"
	importPath := filepath.Join(repoName, "foo")
	util.WriteFile("test.spec", []byte(fmt.Sprintf("(import %q)", importPath)), 0644)

	logger := newRepoLogger()
	getter := ImportGetter{
		Path:        ".",
		repoFactory: logger.newRepoFactory(nil),
	}

	expErr := fmt.Sprintf("unable to open import %[1]s (path=%[1]s.spec)", importPath)
	err := getter.checkSpec("test.spec", nil, nil)
	if err == nil || err.Error() != expErr {
		t.Errorf("Wrong error, expected %q, got %v", expErr, err)
		return
	}

	assert.Equal(t, map[string][]string{
		importPath: {repoName},
	}, logger.created, "Should autodownload the repo")
	assert.Empty(t, logger.updated, "Shouldn't update any repos")
}

func TestGetCreate(t *testing.T) {
	util.AppFs = afero.NewMemMapFs()

	quiltPath := "./getspecs"
	repoName := "github.com/NetSys/quilt"
	importPath := filepath.Join(repoName, "foo")

	logger := newRepoLogger()
	getter := ImportGetter{
		Path:        quiltPath,
		repoFactory: logger.newRepoFactory(nil),
	}

	if err := getter.Get(importPath); err != nil {
		t.Error(err)
		return
	}

	assert.Equal(t, map[string][]string{
		importPath: {filepath.Join(quiltPath, repoName)},
	}, logger.created, "Should download the repo")
	assert.Empty(t, logger.updated, "Shouldn't update any repos")
}

func TestGetWalk(t *testing.T) {
	util.AppFs = afero.NewMemMapFs()

	quiltPath := "getspecs"

	repoOne := "repoOne"
	importOne := filepath.Join(repoOne, "file")

	repoTwo := "anotherRepo"
	fileTwo := "importTwo"
	importTwo := filepath.Join(repoTwo, fileTwo)

	logger := newRepoLogger()
	getter := ImportGetter{
		Path: quiltPath,
		repoFactory: logger.newRepoFactory(map[string][]file{
			importOne: {
				{
					name:     "foo.spec",
					contents: fmt.Sprintf("(import %q)", importTwo),
				},
			},
			importTwo: {
				{
					name: fmt.Sprintf("%s.spec", fileTwo),
				},
			},
		}),
	}

	if err := getter.Get(importOne); err != nil {
		t.Error(err)
		return
	}

	assert.Equal(t, map[string][]string{
		importOne: {filepath.Join(quiltPath, repoOne)},
		importTwo: {filepath.Join(quiltPath, repoTwo)},
	}, logger.created, "Should autodownload the imported repo")
	assert.Empty(t, logger.updated, "Shouldn't update any repos")
}

func TestGetUpdate(t *testing.T) {
	util.AppFs = afero.NewMemMapFs()

	quiltPath := "./getspecs"
	repoName := "github.com/NetSys/quilt"
	util.AppFs.Mkdir(filepath.Join(quiltPath, repoName), 755)

	logger := newRepoLogger()
	getter := ImportGetter{
		Path:        quiltPath,
		repoFactory: logger.newRepoFactory(nil),
	}

	importPath := filepath.Join(repoName, "foo")
	if err := getter.Get(importPath); err != nil {
		t.Error(err)
		return
	}

	assert.Empty(t, logger.created, "Shouldn't create any repos")
	assert.Equal(t, map[string][]string{
		importPath: {filepath.Join(quiltPath, repoName)},
	}, logger.updated, "Should update the repo")
}
