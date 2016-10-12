package stitch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestGetGithubKeys(t *testing.T) {
	expected := []string{"key1", "key2", "key3"}
	handlerFunc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, strings.Join(expected, "\n"))
	})
	ts := httptest.NewServer(handlerFunc)
	defer ts.Close()

	actual, err := getGithubKeys(ts.URL)
	if err != nil {
		t.Error(err)
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("expected %s \n but got %s", expected, actual)
	}

	actual, err = getGithubKeys("Not a URL")
	if actual != nil || err == nil {
		t.Errorf("expected error did not occur")
	}
}
