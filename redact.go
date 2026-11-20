package ccleft

import (
	"net/http"
	"strings"
)

// snippet renders a short single-line body excerpt for error messages. The
// quota endpoints do not echo credentials; bearer-looking tokens are masked
// anyway.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return maskSecrets(s)
}

func maskSecrets(s string) string {
	for _, pfx := range []string{"Bearer ", "ksk_", "sk-", "ghp_", "gho_", "ghu_", "github_pat_"} {
		for {
			i := strings.Index(s, pfx)
			if i < 0 {
				break
			}
			j := i + len(pfx)
			for j < len(s) && s[j] != ' ' && s[j] != '"' && s[j] != ',' {
				j++
			}
			s = s[:i] + "<redacted>" + s[j:]
		}
	}
	return s
}

func redactURL(req *http.Request) string {
	u := *req.URL
	u.RawQuery = ""
	u.User = nil
	return u.String()
}
