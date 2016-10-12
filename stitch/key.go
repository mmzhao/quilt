package stitch

import (
	"io/ioutil"
	"net/http"
	"strings"
)

type key interface {
	keys() ([]string, error)

	ast
}

var githubCache = make(map[string][]string)
var httpGet = http.Get

func (githubKey astGithubKey) keys() ([]string, error) {
	username := string(githubKey)
	if keys, ok := githubCache[username]; ok {
		return keys, nil
	}
	keys, err := getGithubKeys("https://github.com/" + username + ".keys")
	if err != nil {
		return nil, err
	}
	githubCache[username] = keys
	return keys, nil
}

func (plaintextKey astPlaintextKey) keys() ([]string, error) {
	return []string{string(plaintextKey)}, nil
}

func getGithubKeys(keyURL string) ([]string, error) {
	res, err := httpGet(keyURL)
	if err != nil {
		return nil, err
	}
	keyBytes, err := ioutil.ReadAll(res.Body)
	defer res.Body.Close()
	if err != nil {
		return nil, err
	}
	keys := strings.TrimSpace(string(keyBytes))
	keyStrings := strings.Split(keys, "\n")
	return keyStrings, nil
}
