package builtin

import (
	"regexp"
	"sync"
)

var (
	reCacheMu sync.Mutex
	reCache   = make(map[string]*regexp.Regexp)
)

func compileCached(pat string) (*regexp.Regexp, error) {
	reCacheMu.Lock()
	defer reCacheMu.Unlock()
	if re, ok := reCache[pat]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	reCache[pat] = re
	return re, nil
}

func matchRegex(pat, s string) (bool, error) {
	re, err := compileCached(pat)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}
