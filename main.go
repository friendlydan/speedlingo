package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/github"
	"github.com/juju/errors"
	"golang.org/x/oauth2"
	git "gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	yaml "gopkg.in/yaml.v2"
)

type config struct {
	Username string `yaml:"username"`
	Email    string `yaml:"email"`
	Token    string `yaml:"token"`
}

const yamlDataReview = `tenets:
  - import: codelingo/code-review-comments
  - import: codelingo/effective-go
`
const yamlDataRewrite = `tenets:
  - import: codelingo/effective-go/comment-first-word-as-subject
`

const configFile = "config.yaml"
const ignoreData = `vendor/`
const yamlName = "codelingo.yaml"
const ignoreFileName = ".codelingoignore"
const commitMessageRewrite = "Update comments based on best practices from Effective Go"
const branchName = "rewrite"

var reviewResultsDir = os.Getenv("HOME") + "/speedlingo-review-results"

var conf config

func main() {
	var rf *github.Repository
	var err error
	ctx := context.Background()
	if len(os.Args) != 4 {
		fmt.Println("Usage: speedlingo <command> <owner> <repo name>")
		os.Exit(1)
	}

	if err = os.MkdirAll(reviewResultsDir, 0755); err != nil {
		log.Fatal(err)
	}

	conf, err = unmarshalConfigFile()
	if err != nil {
		log.Fatal(err)
	}

	owner := os.Args[2]
	repo := os.Args[3]

	authedClient := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: conf.Token}))
	client := github.NewClient(authedClient)

	rf, _, err = client.Repositories.CreateFork(ctx, owner, repo, nil)
	if err != nil {
		if !strings.Contains(err.Error(), "job scheduled on GitHub side; try again later") {
			log.Fatal(err)
		}
	}

	timeout := time.Now().Add(time.Minute * 5)
	for {
		if time.Now().After(timeout) {
			log.Fatal(err)
		}

		rf, _, err = client.Repositories.Get(ctx, conf.Username, repo)
		if err != nil {
			fmt.Println(err.Error())
			time.Sleep(time.Second * 2)
			continue
		}
		break
	}

	fmt.Println("Forked")

	// Tempdir to clone the repository
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir) // clean up

	fmt.Println("Created temp dir")
	fmt.Println("Attempting to clone", *rf.HTMLURL)
	// Clones the repository into the given dir, just as a normal git clone does
	r, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL:      *rf.HTMLURL,
		Progress: os.Stdout,
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Cloned to", dir)

	var cmd *exec.Cmd
	switch command := os.Args[1]; command {
	case "review":
		fmt.Println("Results will be stored in", reviewResultsDir)
		cmd = exec.Command("lingo", "run", "review", "--debug", "--keep-all", "-o", reviewResultsDir+"/"+repo+"-"+"results.json")
		if err := handleReview(dir, conf.Token, r, cmd); err != nil {
			log.Fatal(err)
		}
	case "rewrite":
		cmd = exec.Command("lingo", "run", "rewrite", "--debug", "--keep-all")
		if err := handleRewrite(dir, conf.Token, r, cmd); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal(errors.New("command not found. Commands available: review, rewrite"))
	}
}

func runCmd(dir string, cmd *exec.Cmd) error {
	err := os.Chdir(dir)
	if err != nil {
		return errors.Trace(err)
	}
	fmt.Println("Running lingo command...")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		os.RemoveAll(dir)
		return errors.Annotate(err, "cmd.Run() failed:")
	}

	return nil
}

func handleRewrite(dir, token string, r *git.Repository, cmd *exec.Cmd) error {
	err := os.Chdir(dir)
	if err != nil {
		return errors.Trace(err)
	}

	worktree, err := r.Worktree()
	if err != nil {
		return errors.Trace(err)
	}

	branch := fmt.Sprintf("refs/heads/%s", branchName)
	b := plumbing.ReferenceName(branch)

	// First try to checkout branch
	err = worktree.Checkout(&git.CheckoutOptions{Create: false, Force: false, Branch: b})

	if err != nil {
		// got an error  - try to create it
		err := worktree.Checkout(&git.CheckoutOptions{Create: true, Force: false, Branch: b})
		if err != nil {
			return errors.Trace(err)
		}
	}

	fmt.Println("Created new branch")

	needsIgnoreFile := false
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return errors.Trace(err)
	}
	for _, file := range files {
		if file.Name() == "vendor" && file.IsDir() {
			fmt.Println("Found vendor directory")
			needsIgnoreFile = true
		}
	}

	filename := filepath.Join(dir, yamlName)
	err = ioutil.WriteFile(filename, []byte(yamlDataRewrite), 0666)
	if err != nil {
		return errors.Trace(err)
	}

	if needsIgnoreFile {
		filename := filepath.Join(dir, ignoreFileName)
		err = ioutil.WriteFile(filename, []byte(ignoreData), 0644)
		if err != nil {
			return errors.Trace(err)
		}
		fmt.Printf("Wrote %s file\n", ignoreFileName)
	}

	fmt.Printf("Wrote %s file\n", yamlName)

	if err = runCmd(dir, cmd); err != nil {
		return errors.Trace(err)
	}

	err = worktree.AddGlob(".")
	if err != nil {
		return errors.Trace(err)
	}
	_, err = worktree.Remove(yamlName)
	if err != nil {
		return errors.Trace(err)
	}

	if needsIgnoreFile {
		_, err = worktree.Remove(ignoreFileName)
		if err != nil {
			return errors.Trace(err)
		}
	}

	commit, err := worktree.Commit(commitMessageRewrite, &git.CommitOptions{
		Author: &object.Signature{
			Name:  conf.Username,
			Email: conf.Email,
			When:  time.Now(),
		},
	})

	_, err = r.CommitObject(commit)
	if err != nil {
		return errors.Trace(err)
	}

	fmt.Println("Committed")

	opt := git.PushOptions{
		RemoteName: "origin",
		Auth: &http.BasicAuth{
			Username: "emptystring", // yes, this can be anything except an empty string
			Password: token,
		},
		Progress: os.Stdout,
	}

	err = r.Push(&opt)
	if err != nil {
		return errors.Trace(err)
	}

	fmt.Println("Pushed")

	return nil
}

func handleReview(dir, token string, r *git.Repository, cmd *exec.Cmd) error {
	needsIgnoreFile := false
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return errors.Trace(err)
	}
	for _, file := range files {
		if file.Name() == "vendor" && file.IsDir() {
			fmt.Println("Found vendor directory")
			needsIgnoreFile = true
		}
	}

	filename := filepath.Join(dir, yamlName)
	err = ioutil.WriteFile(filename, []byte(yamlDataRewrite), 0666)
	if err != nil {
		return errors.Trace(err)
	}

	if needsIgnoreFile {
		filename := filepath.Join(dir, ignoreFileName)
		err = ioutil.WriteFile(filename, []byte(ignoreData), 0644)
		if err != nil {
			return errors.Trace(err)
		}
		fmt.Printf("Wrote %s file\n", ignoreFileName)
	}

	fmt.Printf("Wrote %s file\n", yamlName)
	err = runCmd(dir, cmd)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func unmarshalConfigFile() (config, error) {
	var result config
	str, err := ioutil.ReadFile(configFile)
	if err != nil {
		return config{}, errors.Trace(err)
	}
	err = yaml.UnmarshalStrict([]byte(str), &result)
	return result, nil
}
