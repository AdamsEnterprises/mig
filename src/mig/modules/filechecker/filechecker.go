/* Inspect files on the local system

Version: MPL 1.1/GPL 2.0/LGPL 2.1

The contents of this file are subject to the Mozilla Public License Version
1.1 (the "License"); you may not use this file except in compliance with
the License. You may obtain a copy of the License at
http://www.mozilla.org/MPL/

Software distributed under the License is distributed on an "AS IS" basis,
WITHOUT WARRANTY OF ANY KIND, either express or implied. See the License
for the specific language governing rights and limitations under the
License.

The Initial Developer of the Original Code is
Mozilla Corporation
Portions created by the Initial Developer are Copyright (C) 2013
the Initial Developer. All Rights Reserved.

Contributor(s):
Julien Vehent jvehent@mozilla.com [:ulfr]

Alternatively, the contents of this file may be used under the terms of
either the GNU General Public License Version 2 or later (the "GPL"), or
the GNU Lesser General Public License Version 2.1 or later (the "LGPL"),
in which case the provisions of the GPL or the LGPL are applicable instead
of those above. If you wish to allow use of your version of this file only
under the terms of either the GPL or the LGPL, and not to allow others to
use your version of this file under the terms of the MPL, indicate your
decision by deleting the provisions above and replace them with the notice
and other provisions required by the GPL or the LGPL. If you do not delete
the provisions above, a recipient may use your version of this file under
the terms of any one of the MPL, the GPL or the LGPL.
*/
package filechecker

import (
	"bufio"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"code.google.com/p/go.crypto/sha3"
)

var DEBUG bool = false

// A checklist is an object that has the following representation:
// Parameters {
//	path "path1" {
//		method "name1" {
//			check "id1" [
//				test "value1"
//				test "value2"
//				...
//			],
//			check "id2" [
//				test "value3"
//			]
//		}
//		method "name 2" {
//			...
//		}
//	}
//	path "path2" {
//		...
//	}
// }
//
// In JSON form, the structure above looks like the following:
// {
//	"/some/path/or/file": {
//		"<method=[filename|regex|md5|sha256|...]>": {
//			"random string as identifier": [
//				"^testregex1$",
//				"^.+[0-9][a-z]",
//				.....
//			]
//		}
//	},
//	"/some/other/path":{
//		etc...
//	}
// }
type Parameters map[string]map[string]map[string][]string

// Create a new Parameters
func NewParameters() *Parameters {
	p := make(Parameters)
	return &p
}

// validate a Parameters
func (p Parameters) Validate() (err error) {
	for path, methods := range p {
		if string(path) == "" {
			return fmt.Errorf("Invalid path parameter. Expected string")
		}
		for method, identifiers := range methods {
			if string(method) == "" {
				return fmt.Errorf("Invalid method parameter. Expected string")
			}
			switch method {
			case "filename", "regex", "md5", "sha1", "sha256", "sha384", "sha512",
				"sha3_224", "sha3_256", "sha3_384", "sha3_512":
				err = nil
			default:
				return fmt.Errorf("Invalid method '%s'", method)
			}
			for identifier, tests := range identifiers {
				if string(identifier) == "" {
					return fmt.Errorf("Invalid identifier parameter. Expected string")
				}
				for _, test := range tests {
					if string(test) == "" {
						return fmt.Errorf("Invalid test parameter. Expected string")
					}
				}
			}
		}
	}
	return
}

/* Statistic counters:
- CheckCount is the total numbers of checklist tested
- FilesCount is the total number of files inspected
- Checksmatch is the number of checks that matched at least once
- YniqueFiles is the number of files that matches at least one Check once
- Totalhits is the total number of checklist hits
*/
type statistics struct {
	Checkcount  int    `json:"checkcount"`
	Filescount  int    `json:"filescount"`
	Openfailed  int    `json:"openfailed"`
	Checksmatch int    `json:"checksmatch"`
	Uniquefiles int    `json:"uniquefiles"`
	Totalhits   int    `json:"totalhits"`
	Exectime    string `json:"exectime"`
}

// stats is a global variable
var stats statistics

// Representation of a filecheck.
// id is a string that identifies the check
// path is the file system path to inspect
// method is the name of the type of check
// test is the value of the check, such as a md5 hash
// testcode is the type of test in integer form
// filecount is the total number of files inspected for each Check
// matchcount is a counter of positive results for this Check
// hasmatched is a boolean set to True when the Check has matched once or more
// files is an slice of string that contains paths of matching files
// regex is a regular expression
type filecheck struct {
	id, path, method, test          string
	testcode, filecount, matchcount int
	hasmatched                      bool
	files                           map[string]int
	regex                           *regexp.Regexp
}

// Response is a struct that formats the data returned to the caller
type Results struct {
	FoundAnything bool                                                     `json:"foundanything"`
	Elements      map[string]map[string]map[string]map[string]singleresult `json:"elements"`
	Statistics    statistics                                               `json:"statistics"`
	Error         string                                                   `json:"error"`
}

// singleresult contains information on the result of a single test
type singleresult struct {
	Filecount  int            `json:"filecount"`
	Matchcount int            `json:"matchcount"`
	Files      map[string]int `json:"files"`
}

// NewResponse constructs a Response
func NewResults() *Results {
	return &Results{Elements: make(map[string]map[string]map[string]map[string]singleresult), FoundAnything: false}
}

// Run() is filechecker's entry point. It parses command line arguments into a list of
// individual checks, stored in a map.
// Each Check contains a path, which is inspected in the pathWalk function.
// The results are stored in the checklist map and sent to stdout at the end.
func Run(Args []byte) (resStr string) {
	defer func() {
		if e := recover(); e != nil {
			// return error in json
			res := NewResults()
			res.Statistics = stats
			res.Error = fmt.Sprintf("%v", e)
			err, _ := json.Marshal(res)
			resStr = string(err[:])
			return
		}
	}()
	t0 := time.Now()
	params := NewParameters()
	err := json.Unmarshal(Args, &params)
	if err != nil {
		panic(err)
	}

	err = params.Validate()
	if err != nil {
		panic(err)
	}

	// walk through the parameters and generate a checklist of filechecks
	checklist := make(map[int]filecheck)
	todolist := make(map[int]filecheck)
	interestedlist := make(map[int]filecheck)
	i := 0
	for path, methods := range *params {
		for method, identifiers := range methods {
			for identifier, tests := range identifiers {
				for _, test := range tests {
					check, err := createCheck(path, method, identifier, test)
					if err != nil {
						panic(err)
					}
					checklist[i] = check
					todolist[i] = check
					i++
					stats.Checkcount++
				}
			}
		}
	}

	// loop through the list of checks and initiate a path walk. A given path is walked
	// only once, and the pathwalk function will build a list of checks to run when entering
	// a new path.
	for id, check := range checklist {
		if DEBUG {
			fmt.Printf("Main: Inspecting path '%s' from Check id '%d'\n", check.path, id)
		}
		// loop through the list of checks, and only process the ones that
		// are still in the todo list
		if _, ok := todolist[id]; !ok {
			// this check isn't in the todolist anymore, skip it
			if DEBUG {
				fmt.Printf("Main: Skipping check id '%d'. Already done.\n", id)
			}
			continue
		}
		root := findRootPath(check.path)
		if DEBUG {
			fmt.Printf("Main: Found root path at '%s'\n", root)
		}
		err = pathWalk(root, checklist, todolist, interestedlist)
		if err != nil {
			panic(err)
			if DEBUG {
				fmt.Printf("pathWalk failed with error '%v'\n", err)
			}
		}
	}

	resStr, err = buildResults(checklist, t0)
	if err != nil {
		panic(err)
	}
	return
}

// BitMask for the type of check to apply to a given file
// see documentation about iota for more info
const (
	CheckRegex = 1 << iota
	CheckFileName
	CheckMD5
	CheckSHA1
	CheckSHA256
	CheckSHA384
	CheckSHA512
	CheckSHA3_224
	CheckSHA3_256
	CheckSHA3_384
	CheckSHA3_512
)

// createCheck creates a new filecheck
func createCheck(path, method, identifier, test string) (check filecheck, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("createCheck() -> %v", e)
		}
	}()
	check.id = identifier
	check.path = path
	check.method = method
	check.test = test
	switch method {
	case "regex":
		check.testcode = CheckRegex
		// compile the value into a regex
		check.regex = regexp.MustCompile(test)
	case "filename":
		check.testcode = CheckFileName
		// compile the value into a regex
		check.regex = regexp.MustCompile(test)
	case "md5":
		check.testcode = CheckMD5
	case "sha1":
		check.testcode = CheckSHA1
	case "sha256":
		check.testcode = CheckSHA256
	case "sha384":
		check.testcode = CheckSHA384
	case "sha512":
		check.testcode = CheckSHA512
	case "sha3_224":
		check.testcode = CheckSHA3_224
	case "sha3_256":
		check.testcode = CheckSHA3_256
	case "sha3_384":
		check.testcode = CheckSHA3_384
	case "sha3_512":
		check.testcode = CheckSHA3_512
	default:
		err := fmt.Sprintf("ParseCheck: Invalid method '%s'", method)
		panic(err)
	}
	// allocate the map
	check.files = make(map[string]int)
	// init the variables
	check.hasmatched = false
	check.filecount = 0
	check.matchcount = 0
	return
}

// findRootPath takes a path pattern and extracts the root, that is the
// directory we can start our pattern search from.
// example: pattern='/etc/cron.*/*' => root='/etc/'
func findRootPath(pattern string) string {
	// find the root path before the first pattern character.
	// seppos records the position of the latest path separator
	// before the first pattern.
	seppos := 1
	for cursor := 0; cursor < len(pattern); cursor++ {
		char := pattern[cursor]
		switch char {
		case '*', '?', '[', '{':
			// found pattern character. but ignore it if preceded by backslash
			if cursor > 0 {
				if pattern[cursor-1] == '\\' {
					break
				}
			}
			goto exit
		case os.PathSeparator:
			if cursor > 0 {
				seppos = cursor
			}
		}
	}
exit:
	return pattern[0 : seppos+1]
}

// pathWalk goes down a directory and build a list of Active checklist that
// apply to the current path. For a given directory, it calls itself for all
// subdirectories fund, recursively walking down the pass. When it find a file,
// it calls the inspection function, and give it the list of checklist to inspect
// the file with.
// parameters:
//      - path is the file system path to inspect
//      - checklist is the global list of checklist
//      - todolist is a map that contains the checklist that are not yet active
//      - interestedlist is a map that contains checks that are interested in the
//	  current path but not yet active
// return:
//      - nil on success, error on error
func pathWalk(path string, checklist, todolist, interestedlist map[int]filecheck) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("pathWalk() -> %v", e)
		}
	}()
	for id, check := range todolist {
		if pathIncludes(path, check.path) {
			/* Found a new Check to apply to the current path, add
			   it to the interested list, and delete it from the todo
			*/
			interestedlist[id] = todolist[id]
			delete(todolist, id)
		}
	}
	var subdirs []string
	// Read the content of dir stored in 'path',
	// put all sub-directories in the SubDirs slice, and call
	// the inspection function for all files
	target, err := os.Open(path)
	if err != nil {
		stats.Openfailed++
		// do not panic when open fails, just increase a counter
		return nil
	}
	targetMode, _ := target.Stat()
	if targetMode.Mode().IsDir() {
		// target is a directory, process its content
		dirContent, err := target.Readdir(-1)
		if err != nil {
			panic(err)
		}
		// loop over the content of the directory
		for _, dirEntry := range dirContent {
			entryAbsPath := path
			// append path separator if missing
			if entryAbsPath[len(entryAbsPath)-1] != os.PathSeparator {
				entryAbsPath += string(os.PathSeparator)
			}
			entryAbsPath += dirEntry.Name()
			// this entry is a subdirectory, keep it for later
			if dirEntry.IsDir() {
				subdirs = append(subdirs, entryAbsPath)
				continue
			}
			if dirEntry.Mode().IsRegular() {
				err = evaluateFile(entryAbsPath, interestedlist, checklist)
				if err != nil {
					panic(err)
				}
			}
		}
	}
	if targetMode.Mode().IsRegular() {
		err = evaluateFile(path, interestedlist, checklist)
		if err != nil {
			panic(err)
		}
	}
	// close the current target, we are done with it
	if err := target.Close(); err != nil {
		panic(err)
	}
	// if we found any sub directories, go down the rabbit hole recursively,
	// but only if one of the check is interested in going
	for _, dir := range subdirs {
		interested := false
		for _, check := range interestedlist {
			if pathIncludes(dir, check.path) {
				interested = true
				break
			}
		}
		if interested {
			err = pathWalk(dir, checklist, todolist, interestedlist)
			if err != nil {
				panic(err)
			}
		}
	}
	return nil
}

// pathIncludes is an optimization that matches a pattern with the current
// depth of a directory.
// To make filtering more efficient, split the pattern at the PathSeparator
// level of the current path. If the current levels don't match, there's no
// need to continue further down this path
func pathIncludes(root, pattern string) bool {
	rootdepth := 0
	for pos := 0; pos < len(root); pos++ {
		if root[pos] == os.PathSeparator {
			rootdepth++
		}
	}
	subpattern := pattern
	patterndepth := 0
	for pos := 0; pos < len(pattern); pos++ {
		if pattern[pos] == os.PathSeparator {
			patterndepth++
		}
		if patterndepth == rootdepth {
			// pattern reaches the same depth as root, we have two choices:
			// 1. pattern has a match in the current depth, in which case we
			//    use pattern as it is
			// 2. pattern has a match in a subdirectory, so we create a subpattern
			//    that only matches the current depth
			subpattern = pattern[0 : pos+1]
			if -1 != strings.Index(pattern[pos+1:len(pattern)-1], string(os.PathSeparator)) {
				subpattern += "*"
			} else {
				subpattern = pattern
			}
			break
		}
	}
	match, _ := filepath.Match(subpattern, root)
	if !match {
		if DEBUG {
			fmt.Printf("pathIncludes: '%s' is NOT interested in path '%s'\n", subpattern, root)
		}
		return false
	}
	if DEBUG {
		fmt.Printf("pathIncludes: '%s' is interested in path '%s'\n", subpattern, root)
	}
	return true
}

// evaluateFile looks for patterns that match a file and build a list of checks
// passed to inspectFile
func evaluateFile(file string, interestedlist, checklist map[int]filecheck) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("evaluateFile() -> %v", e)
		}
	}()
	if DEBUG {
		fmt.Printf("evaluateFile: evaluating '%s'\n", file)
	}
	// that one is a file, see if it matches one of the pattern
	inspect := false
	checkBitmask := 0
	var activechecks []int
	for id, check := range interestedlist {
		match, err := filepath.Match(check.path, file)
		if err != nil {
			return err
		}
		if match {
			if DEBUG {
				fmt.Printf("evaluateFile: activated check id '%d' '%s' on '%s'\n", id, check.path, file)
			}
			activechecks = append(activechecks, id)
			checkBitmask |= check.testcode
			inspect = true
		}
	}
	if inspect {
		// it matches, open the file and inspect it
		entryfd, err := os.Open(file)
		if err != nil {
			// woops, open failed. update counters and move on
			stats.Openfailed++
			return nil
		}
		inspectFile(entryfd, activechecks, checkBitmask, checklist)
		stats.Filescount++
		if err := entryfd.Close(); err != nil {
			panic(err)
		}
		stats.Filescount++
	}
	return
}

// inspectFile is an orchestration function that runs the individual checks
// against a selected file. It uses checkBitmask to find the checks it needs
// to run. The file is opened once, and all the checks are ran against it,
// minimizing disk IOs.
// parameters:
//      - fd is an open file descriptor that points to the file to inspect
//      - activechecks is a slice that contains the IDs of the checklist
//      that all files in that path and below must be checked against
//      - checkBitmask is a bitmask of the checks types currently active
//      - checklist is the global list of checklist
// returns:
//      - nil on success, error on failure
func inspectFile(fd *os.File, activechecks []int, checkBitmask int, checklist map[int]filecheck) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("inspectFile() -> %v", e)
		}
	}()
	// Iterate through the entire checklist, and process the checks of each file
	if DEBUG {
		fmt.Printf("InspectFile: file '%s' CheckMask '%d'\n",
			fd.Name(), checkBitmask)
	}
	if (checkBitmask & CheckRegex) != 0 {
		// build a list of checklist of check type 'contains'
		var ReList []int
		for _, id := range activechecks {
			if (checklist[id].testcode & CheckRegex) != 0 {
				ReList = append(ReList, id)
			}
		}
		match, err := matchRegexOnFile(fd, ReList, checklist)
		if err != nil {
			panic(err)
		}
		if match {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckFileName) != 0 {
		// build a list of checklist of check type 'contains'
		var ReList []int
		for _, id := range activechecks {
			if (checklist[id].testcode & CheckFileName) != 0 {
				ReList = append(ReList, id)
			}
		}
		if matchRegexOnName(fd.Name(), ReList, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckMD5) != 0 {
		hash, err := getHash(fd, CheckMD5)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckMD5, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA1) != 0 {
		hash, err := getHash(fd, CheckSHA1)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA1, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA256) != 0 {
		hash, err := getHash(fd, CheckSHA256)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA256, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA384) != 0 {
		hash, err := getHash(fd, CheckSHA384)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA384, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA512) != 0 {
		hash, err := getHash(fd, CheckSHA512)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA512, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA3_224) != 0 {
		hash, err := getHash(fd, CheckSHA3_224)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA3_224, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA3_256) != 0 {
		hash, err := getHash(fd, CheckSHA3_256)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA3_256, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA3_384) != 0 {
		hash, err := getHash(fd, CheckSHA3_384)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA3_384, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	if (checkBitmask & CheckSHA3_512) != 0 {
		hash, err := getHash(fd, CheckSHA3_512)
		if err != nil {
			panic(err)
		}
		if verifyHash(fd.Name(), hash, CheckSHA3_512, activechecks, checklist) {
			if DEBUG {
				fmt.Printf("InspectFile: Positive result found for '%s'\n", fd.Name())
			}
		}
	}
	return
}

// getHash calculates the hash of a file.
// It reads a file block by block, and updates a hashsum with each block.
// Reading by blocks consume very little memory, which is needed for large files.
// parameters:
//      - fd is an open file descriptor that points to the file to inspect
//      - hashType is an integer that define the type of hash
// return:
//      - hexhash, the hex encoded hash of the file found at fp
func getHash(fd *os.File, hashType int) (hexhash string, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("getHash() -> %v", e)
		}
	}()
	if DEBUG {
		fmt.Printf("getHash: computing hash for '%s'\n", fd.Name())
	}
	var h hash.Hash
	switch hashType {
	case CheckMD5:
		h = md5.New()
	case CheckSHA1:
		h = sha1.New()
	case CheckSHA256:
		h = sha256.New()
	case CheckSHA384:
		h = sha512.New384()
	case CheckSHA512:
		h = sha512.New()
	case CheckSHA3_224:
		h = sha3.NewKeccak224()
	case CheckSHA3_256:
		h = sha3.NewKeccak256()
	case CheckSHA3_384:
		h = sha3.NewKeccak384()
	case CheckSHA3_512:
		h = sha3.NewKeccak512()
	default:
		err := fmt.Sprintf("getHash: Unkown hash type %d", hashType)
		panic(err)
	}
	buf := make([]byte, 4096)
	var offset int64 = 0
	for {
		block, err := fd.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			panic(err)
		}
		if block == 0 {
			break
		}
		h.Write(buf[:block])
		offset += int64(block)
	}
	hexhash = fmt.Sprintf("%x", h.Sum(nil))
	return
}

// verifyHash compares a file hash with the checklist that apply to the file
// parameters:
//      - file is the absolute filename of the file to check
//      - hash is the value of the hash being checked
//      - check is the type of check
//      - activechecks is a slice of int with IDs of active checklist
//      - checklist is a map of Check
// returns:
//      - IsVerified: true if a match is found, false otherwise
func verifyHash(file string, hash string, check int, activechecks []int, checklist map[int]filecheck) (IsVerified bool) {
	IsVerified = false
	for _, id := range activechecks {
		tmpcheck := checklist[id]
		if checklist[id].test == hash {
			IsVerified = true
			tmpcheck.hasmatched = true
			tmpcheck.matchcount++
			tmpcheck.files[file] = 1
		}
		// update checklist tested files count
		tmpcheck.filecount++
		checklist[id] = tmpcheck
	}
	return
}

// matchRegexOnFile read a file line by line and apply regexp search to each
// line. If a regexp matches, the corresponding Check is updated with the result.
// All regexp are compiled during argument parsing and not here.
// parameters:
//      - fd is a file descriptor on the open file
//      - ReList is a list of Check IDs to apply to this file
//      - checklist is a map of Check
// return:
//      - hasmatched is a boolean set to true if at least one regexp matches
func matchRegexOnFile(fd *os.File, ReList []int, checklist map[int]filecheck) (hasmatched bool, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("matchRegexOnFile() -> %v", e)
		}
	}()
	hasmatched = false
	// temp map to store the results
	results := make(map[int]int)
	scanner := bufio.NewScanner(fd)
	for scanner.Scan() {
		if err := scanner.Err(); err != nil {
			panic(err)
		}
		for _, id := range ReList {
			if checklist[id].regex.MatchString(scanner.Text()) {
				hasmatched = true
				results[id]++
				break
			}
		}
	}
	if hasmatched {
		for id, count := range results {
			tmpcheck := checklist[id]
			tmpcheck.hasmatched = true
			tmpcheck.matchcount += count
			tmpcheck.files[fd.Name()] = count
			checklist[id] = tmpcheck
		}
	}
	// update checklist tested files count
	for _, id := range ReList {
		tmpcheck := checklist[id]
		tmpcheck.filecount++
		checklist[id] = tmpcheck
	}
	return
}

// matchRegexOnName applies regexp search to a given filename
// parameters:
//      - filename is a string that contains a filename
//      - ReList is a list of Check IDs to apply to this file
//      - checklist is a map of Check
// return:
//      - hasmatched is a boolean set to true if at least one regexp matches
func matchRegexOnName(filename string, ReList []int, checklist map[int]filecheck) (hasmatched bool) {
	hasmatched = false
	for _, id := range ReList {
		tmpcheck := checklist[id]
		if checklist[id].regex.MatchString(filename) {
			hasmatched = true
			tmpcheck.hasmatched = true
			tmpcheck.matchcount++
			tmpcheck.files[filename] = tmpcheck.matchcount
		}
		// update checklist tested files count
		tmpcheck.filecount++
		checklist[id] = tmpcheck
	}
	return
}

// buildResults iterates on the map of checklist and print the results to stdout (if
// DEBUG is set) and into JSON format
func buildResults(checklist map[int]filecheck, t0 time.Time) (resStr string, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("buildResults() -> %v", e)
		}
	}()
	res := NewResults()
	history := make(map[string]int)

	// iterate through the checklist and parse the results
	// into a Response object
	for _, check := range checklist {
		if DEBUG {
			fmt.Printf("Main: Check '%s' returned %d positive match\n", check.id, check.matchcount)
		}
		if check.hasmatched {
			for file, hits := range check.files {
				if DEBUG {
					fmt.Printf("\t- %d hits on %s\n", hits, file)
				}
				stats.Totalhits += hits
				if _, ok := history[file]; !ok {
					stats.Uniquefiles++
				}
			}
			stats.Checksmatch++
		}

		// build a single results and insert it into the result structure
		r := singleresult{
			Filecount:  check.filecount,
			Matchcount: check.matchcount,
			Files:      check.files,
		}
		// to avoid overwriting existing elements, we test each level before inserting the result
		if _, ok := res.Elements[check.path]; !ok {
			res.Elements[check.path] = map[string]map[string]map[string]singleresult{
				check.method: map[string]map[string]singleresult{
					check.id: map[string]singleresult{
						check.test: r,
					},
				},
			}
		} else if _, ok := res.Elements[check.path][check.method]; !ok {
			res.Elements[check.path][check.method] = map[string]map[string]singleresult{
				check.id: map[string]singleresult{
					check.test: r,
				},
			}
		} else if _, ok := res.Elements[check.path][check.method][check.id]; !ok {
			res.Elements[check.path][check.method][check.id] = map[string]singleresult{
				check.test: r,
			}
		} else if _, ok := res.Elements[check.path][check.method][check.id][check.test]; !ok {
			res.Elements[check.path][check.method][check.id][check.test] = r
		}
	}

	// if something matched anywhere, set the global boolean to true
	if stats.Checksmatch > 0 {
		res.FoundAnything = true
	}

	// calculate execution time
	t1 := time.Now()
	stats.Exectime = t1.Sub(t0).String()

	// store the stats in the response
	res.Statistics = stats

	if DEBUG {
		fmt.Printf("Tested checklist: %d\n"+
			"Tested files:     %d\n"+
			"checklist Match:  %d\n"+
			"Unique Files:     %d\n"+
			"Total hits:       %d\n"+
			"Execution time:   %s\n",
			stats.Checkcount, stats.Filescount,
			stats.Checksmatch, stats.Uniquefiles,
			stats.Totalhits, stats.Exectime)
	}
	JsonResults, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}
	resStr = string(JsonResults[:])
	return
}
