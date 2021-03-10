package extractor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/mholt/archiver"
)

// TODO auto detect git
// Hint: run "which git" (does this works on Windows?)
const gitExecutable = "/usr/bin/git"

// TODO implement seed (suggested emails)
// TODO handle async errors correctly

// RepoExtractor is responsible for all parts of repo extraction process
// Including cloning the repo, processing the commits and uploading the results
type RepoExtractor struct {
	RepoPath    string
	Headless    bool
	UserEmails  []string
	repo        *repo
	userCommits []*commit // Commits which are belong to user (from selected emails)
}

// Extract a single repo in the path
func (r *RepoExtractor) Extract() error {

	err := r.initRepo()
	if err != nil {
		return err
	}

	err = r.analyseCommits()
	if err != nil {
		return err
	}

	err = r.analyseLibraries()
	if err != nil {
		return err
	}

	err = r.export()
	if err != nil {
		return err
	}

	// Only when user running this script locally
	if !r.Headless {
		r.upload()
	}

	return nil
}

// Creates Repo struct
func (r *RepoExtractor) initRepo() error {
	fmt.Println("Initializing repository")

	cmd := exec.Command(gitExecutable,
		"config",
		"--get",
		"remote.origin.url",
	)
	cmd.Dir = r.RepoPath

	out, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}

	repoName := ""
	remoteOrigin := string(out)

	// TODO error handling

	// Cloned using http
	if strings.Contains(remoteOrigin, "http") {
		parts := strings.Split(remoteOrigin, "/")
		repoName = parts[len(parts)-2] + "/" + parts[len(parts)-1]
	} else {
		// Cloned using ssh
		parts := strings.Split(remoteOrigin, ":")
		parts = strings.Split(parts[1], ".git")
		repoName = parts[0]
	}

	r.repo = &repo{
		Repo:             repoName,
		Emails:           []string{},
		SuggestedEmails:  []string{}, // TODO implement
		PrimaryRemoteURL: string(out),
	}
	return nil
}

// Creates commits
func (r *RepoExtractor) analyseCommits() error {
	fmt.Println("Analysing commits")

	var commits []*commit
	commits, err := r.getCommits()
	if err != nil {
		return err
	}

	selectedEmails := make(map[string]bool)
	if len(r.UserEmails) == 0 {
		// Ask user emails
		// TODO sort by alphabetical order (or frequency?)
		// TODO use seeds
		allEmails := make([]string, 0, len(commits))
		emails := make(map[string]bool)
		for _, v := range commits {
			if _, ok := emails[v.AuthorEmail]; !ok {
				emails[v.AuthorEmail] = true
				allEmails = append(allEmails, fmt.Sprintf("%s -> %s", v.AuthorName, v.AuthorEmail))
			}
		}

		selectedEmailsWithNames := []string{}
		prompt := &survey.MultiSelect{
			Message:  "Please choose your emails:",
			Options:  allEmails,
			PageSize: 50,
		}
		survey.AskOne(prompt, &selectedEmailsWithNames)

		selectedEmails = make(map[string]bool, len(selectedEmailsWithNames))
		selectedEmailsArray := make([]string, len(selectedEmailsWithNames))
		for i, selectedEmail := range selectedEmailsWithNames {
			fields := strings.Split(selectedEmail, " -> ")
			// TODO handle authorName being empty
			if len(fields) > 0 {
				selectedEmails[fields[1]] = true
				selectedEmailsArray[i] = fields[1]
			}
		}
		r.repo.Emails = selectedEmailsArray
	} else {
		r.repo.Emails = r.UserEmails
		selectedEmails = make(map[string]bool, len(r.UserEmails))
		for _, email := range r.UserEmails {
			selectedEmails[email] = true
		}
	}

	// Only consider commits for user
	userCommits := make([]*commit, 0, len(commits))
	for _, v := range commits {
		if _, ok := selectedEmails[v.AuthorEmail]; ok {
			userCommits = append(userCommits, v)
		}
	}

	r.userCommits = userCommits
	return nil
}

func (r *RepoExtractor) getCommits() ([]*commit, error) {
	jobs := make(chan *req)
	results := make(chan []*commit)
	noMoreChan := make(chan bool)
	for w := 0; w < runtime.NumCPU(); w++ {
		go r.commitWorker(w, jobs, results, noMoreChan)
	}

	// launch initial jobs
	lastOffset := 0
	step := 1000
	for x := 0; x < runtime.NumCPU(); x++ {
		jobs <- &req{
			Limit:  step,
			Offset: x * step,
		}
		lastOffset = step * x
	}

	var commits []*commit
	workersReturnedNoMore := 0
	func() {
		for {
			select {
			case res := <-results:
				lastOffset += step
				jobs <- &req{
					Limit:  step,
					Offset: lastOffset,
				}
				commits = append(commits, res...)
			case <-noMoreChan:
				workersReturnedNoMore++
				if workersReturnedNoMore == runtime.NumCPU() {
					close(jobs)
					return
				}
			}
		}
	}()

	return commits, nil
}

// commitWorker get commits from git
func (r *RepoExtractor) commitWorker(w int, jobs <-chan *req, results chan<- []*commit, noMoreChan chan<- bool) error {
	for v := range jobs {
		var commits []*commit

		cmd := exec.Command(gitExecutable,
			"log",
			"--numstat",
			fmt.Sprintf("--skip=%d", v.Offset),
			fmt.Sprintf("--max-count=%d", v.Limit),
			"--pretty=format:|||BEGIN|||%H|||SEP|||%an|||SEP|||%ae|||SEP|||%ad",
			"--no-merges",
		)
		cmd.Dir = r.RepoPath
		stdout, err := cmd.StdoutPipe()
		if nil != err {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}

		// parse the output into stats
		scanner := bufio.NewScanner(stdout)
		currentLine := 0
		var currectCommit *commit
		for scanner.Scan() {
			m := scanner.Text()
			currentLine++
			if m == "" {
				continue
			}
			if strings.HasPrefix(m, "|||BEGIN|||") {
				// we reached a new commit
				// save the existing
				if currectCommit != nil {
					commits = append(commits, currectCommit)
				}

				// and add new one commit
				m = strings.Replace(m, "|||BEGIN|||", "", 1)
				bits := strings.Split(m, "|||SEP|||")
				changedFiles := []*changedFile{}
				currectCommit = &commit{
					Hash:         bits[0],
					AuthorName:   bits[1],
					AuthorEmail:  bits[2],
					Date:         bits[3],
					ChangedFiles: changedFiles,
				}
				continue
			}

			bits := strings.Fields(m)

			insertionsString := bits[0]
			if insertionsString == "-" {
				insertionsString = "0"
			}
			insertions, err := strconv.Atoi(insertionsString)
			if err != nil {
				return err
			}

			deletionsString := bits[1]
			if deletionsString == "-" {
				deletionsString = "0"
			}
			deletions, err := strconv.Atoi(deletionsString)
			if err != nil {
				return err
			}

			fileName := bits[2]
			// it is a rename, skip
			if strings.Contains("=>", fileName) {
				continue
			}

			changedFile := &changedFile{
				Path:       bits[2],
				Insertions: insertions,
				Deletions:  deletions,
			}

			if currectCommit == nil {
				// TODO maybe skip? does this break anything?
				return errors.New("did not expect currect commit to be null")
			}

			if currectCommit.ChangedFiles == nil {
				// TODO maybe skip? does this break anything?
				return errors.New("did not expect currect commit changed files to be null")
			}

			currectCommit.ChangedFiles = append(currectCommit.ChangedFiles, changedFile)
		}

		// last commit will not get appended otherwise
		// because scanner is not returning anything
		if currectCommit != nil {
			commits = append(commits, currectCommit)
		}

		if len(commits) == 0 {
			noMoreChan <- true
			return nil
		}
		results <- commits
	}
	return nil
}

// TODO This is not ready yet (can't find libraries based on language -> look at libraryWorker)
func (r *RepoExtractor) analyseLibraries() error {
	fmt.Println("Analysing libraries")

	jobs := make(chan *commit, len(r.userCommits))
	results := make(chan bool, len(r.userCommits))
	// Analyse libraries for every commit
	for w := 1; w <= runtime.NumCPU(); w++ {
		go r.libraryWorker(jobs, results)
	}
	for _, v := range r.userCommits {
		jobs <- v
	}
	close(jobs)
	for a := 1; a <= len(r.userCommits); a++ {
		<-results
	}
	return nil
}

func (r *RepoExtractor) libraryWorker(jobs <-chan *commit, results chan<- bool) error {
	extensionToLanguageMap := buildExtensionToLanguageMap(fileExtensionMap)
	for v := range jobs {
		for n, fileChange := range v.ChangedFiles {
			extension := filepath.Ext(fileChange.Path)
			if extension == "" {
				continue
			}
			// remove the trailing dot
			extension = extension[1:]
			lang, ok := extensionToLanguageMap[extension]
			// We don't know extension, nothing to do
			if !ok {
				continue
			}

			// Detect language
			// TODO implement a solution for cases we can't rely on extension
			// For example for Matlab / Objective-C
			v.ChangedFiles[n].Language = lang

			cmd := exec.Command(gitExecutable,
				"show",
				fmt.Sprintf("%s:%s", v.Hash, fileChange.Path),
			)
			cmd.Dir = r.RepoPath

			out, err := cmd.CombinedOutput()
			if err != nil {
				searchString1 := fmt.Sprintf("Path '%s' does not exist in '%s'", fileChange.Path, v.Hash)
				searchString2 := fmt.Sprintf("Path '%s' exists on disk, but not in '%s'", fileChange.Path, v.Hash)
				// means the file was deleted, skip
				if strings.Contains(string(out), searchString1) || strings.Contains(string(out), searchString2) {
					continue
				}
				return err
			}

			// We shouldn't do the following (remove it)
			// We should wrote regexes based on language and run it according to the extension
			// Like we do in old repo_info_extractor

			// run some regexes
			r1 := regexp.MustCompile("[aA-zZ]{3}\\s[0-9]{2}\\s[aA-zZ]{3}\\s[0-9]{4}")
			r1Results := r1.FindAllString(string(out), -1)
			if len(r1Results) > 0 {
				// fmt.Printf("[1]Found the following in %s: %+v", fileChange.Path, r1Results)
			}
			r2 := regexp.MustCompile(`\[([^\[\]]*)\]`)
			r2Results := r2.FindAllString(string(out), -1)
			if len(r2Results) > 0 {
				// fmt.Printf("[2]Found the following in %s: %+v", fileChange.Path, r2Results)
			}
			// v.ChangedFiles[n].Libraries = make([]string, len(r1Results)+len(r2Results))
			// v.ChangedFiles[n].Libraries = append(v.ChangedFiles[n].Libraries, r1Results...)
			// v.ChangedFiles[n].Libraries = append(v.ChangedFiles[n].Libraries, r2Results...)
		}
		results <- true
	}
	return nil
}

// Writes result to the file
func (r *RepoExtractor) export() error {
	fmt.Println("Creating output file")

	// Remove old files
	os.Remove("./repo.data")
	os.Remove("./repo.data.zip")

	file, err := os.Create("./repo.data")
	if err != nil {
		return err
	}

	w := bufio.NewWriter(file)
	repoMetaData, err := json.Marshal(r.repo)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, string(repoMetaData))

	for _, commit := range r.userCommits {
		commitData, err := json.Marshal(commit)
		if err != nil {
			fmt.Printf("Couldn't write commit to file. CommitHash: %s Error: %s", commit.Hash, err.Error())
			continue
		}
		fmt.Fprintln(w, string(commitData))
	}
	w.Flush() // important
	file.Close()

	err = archiver.Archive([]string{"./repo.data"}, "./repo.data.zip")
	if err != nil {
		return err
	}

	// We don't need this because we already have zip file
	os.Remove("./repo.data")

	return nil
}

// TODO implement
// This is for repo_info_extractor used locally and for user to
// upload his/her results automatically to the codersrank
func (r *RepoExtractor) upload() {

}

type repo struct {
	Repo             string   `json:"repo"`
	Emails           []string `json:"emails"`
	SuggestedEmails  []string `json:"suggestedEmails"`
	PrimaryRemoteURL string   `json:"primaryRemoteUrl"`
}

type changedFile struct {
	Path       string              `json:"fileName"`
	Insertions int                 `json:"insertions"`
	Deletions  int                 `json:"deletions"`
	Language   string              `json:"language"`
	Libraries  map[string][]string `json:"libraries"`
}

type commit struct {
	Hash         string         `json:"commitHash"`
	AuthorName   string         `json:"authorName"`
	AuthorEmail  string         `json:"authorEmail"`
	Date         string         `json:"createdAt"`
	ChangedFiles []*changedFile `json:"changedFiles"`
}

type req struct {
	Limit  int
	Offset int
}

func buildExtensionToLanguageMap(input map[string][]string) map[string]string {
	extensionMap := map[string]string{}
	for lang, extensions := range input {
		for _, extension := range extensions {
			extensionMap[extension] = lang
		}
	}
	return extensionMap
}

var fileExtensionMap = map[string][]string{
	"1C Enterprise":    {"bsl", "os"},
	"Apex":             {"cls"},
	"Assembly":         {"asm"},
	"Batchfile":        {"bat", "cmd", "btm"},
	"C":                {"c", "h"},
	"C++":              {"cpp", "cxx", "hpp", "cc", "hh", "hxx"},
	"C#":               {"cs"},
	"CSS":              {"css"},
	"Clojure":          {"clj"},
	"COBOL":            {"cbl", "cob", "cpy"},
	"CoffeeScript":     {"coffee"},
	"Crystal":          {"cr"},
	"Dart":             {"dart"},
	"Groovy":           {"groovy", "gvy", "gy", "gsh"},
	"HTML+Razor":       {"cshtml"},
	"EJS":              {"ejs"},
	"Elixir":           {"ex", "exs"},
	"Elm":              {"elm"},
	"EPP":              {"epp"},
	"ERB":              {"erb"},
	"Erlang":           {"erl", "hrl"},
	"F#":               {"fs", "fsi", "fsx", "fsscript"},
	"Fortran":          {"f90", "f95", "f03", "f08", "for"},
	"Go":               {"go"},
	"Haskell":          {"hs", "lhs"},
	"HCL":              {"hcl", "tf", "tfvars"},
	"HTML":             {"html", "htm", "xhtml"},
	"JSON":             {"json"},
	"Java":             {"java"},
	"JavaScript":       {"js", "jsx", "mjs", "cjs"},
	"Jupyter Notebook": {"ipynb"},
	"Kivy":             {"kv"},
	"Kotlin":           {"kt", "kts"},
	"Less":             {"less"},
	"Lex":              {"l"},
	"Liquid":           {"liquid"},
	"Lua":              {"lua"},
	"MATLAB":           {"m"},
	"Nix":              {"nix"},
	"Objective-C":      {"mm"},
	"OpenEdge ABL":     {"p", "ab", "w", "i", "x"},
	"Perl":             {"pl", "pm", "t"},
	"PHP":              {"php"},
	"PLSQL":            {"pks", "pkb"},
	"Protocol Buffer":  {"proto"},
	"Puppet":           {"pp"},
	"Python":           {"py"},
	"QML":              {"qml"},
	"R":                {"r"},
	"Raku":             {"p6", "pl6", "pm6", "rk", "raku", "pod6", "rakumod", "rakudoc"},
	"Robot":            {"robot"},
	"Ruby":             {"rb"},
	"Rust":             {"rs"},
	"Scala":            {"scala"},
	"SASS":             {"sass"},
	"SCSS":             {"scss"},
	"Shell":            {"sh"},
	"Smalltalk":        {"st"},
	"Stylus":           {"styl"},
	"Svelte":           {"svelte"},
	"Swift":            {"swift"},
	"TypeScript":       {"ts", "tsx"},
	"Vue":              {"vue"},
	"Xtend":            {"xtend"},
	"Xtext":            {"xtext"},
	"Yacc":             {"y"},
}
