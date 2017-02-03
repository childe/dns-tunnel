package main

import (
	"regexp"
	"testing"
)

func TestRegexp(t *testing.T) {
	hostPattern, err := regexp.Compile(`(?:[A-Za-z]+(?:\+[A-Za-z+]+)?://)?(?:[a-zA-Z0-9._-]+(?::[^@]*)?@)?\b([^/?#]+)\b`)
	if err != nil {
		t.Errorf("could not compile regexp:%s", err)
	}

	var url string
	var host [][]byte

	url = "http://www.baidu.com"
	host = hostPattern.FindSubmatch([]byte(url))
	if string(host[1]) != "www.baidu.com" {
		t.Errorf("could not get correct host from %s. it returned [%s]", url, host)
	}

	url = "http://www.baidu.com/"
	host = hostPattern.FindSubmatch([]byte(url))
	if string(host[1]) != "www.baidu.com" {
		t.Errorf("could not get correct host from %s. it returned [%s]", url, host)
	}

	url = "http://www.baidu.com?q=abcd"
	host = hostPattern.FindSubmatch([]byte(url))
	if string(host[1]) != "www.baidu.com" {
		t.Errorf("could not get correct host from %s. it returned [%s]", url, host)
	}

	url = "http://www.baidu.com/?q=abcd"
	host = hostPattern.FindSubmatch([]byte(url))
	if string(host[1]) != "www.baidu.com" {
		t.Errorf("could not get correct host from %s. it returned [%s]", url, host)
	}

	url = "www.baidu.com"
	host = hostPattern.FindSubmatch([]byte(url))
	if string(host[1]) != "www.baidu.com" {
		t.Errorf("could not get correct host from %s. it returned [%s]", url, host)
	}
}
