package main

import (
	"strconv"
	"strings"
)

func goversion1(goversion string) (version1 int, ok bool) {
	if !strings.HasPrefix(goversion, "go1.") {
		return 0, false
	}
	s := goversion[len("go1."):]
	s = strings.SplitN(s, "rc", 2)[0]
	s = strings.SplitN(s, "beta", 2)[0]
	s = strings.SplitN(s, ".", 2)[0]
	if v, err := strconv.ParseInt(s, 10, 32); err != nil {
		return 0, false
	} else {
		return int(v), true
	}
}
