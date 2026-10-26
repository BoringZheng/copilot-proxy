package common

import (
	"net/url"
	"strings"
)

// URLOriginInList matches a URL against comma-separated, exact scheme/host/port
// origins. Paths, query strings and hostname prefixes never widen the scope.
// A single origin retains the original compatibility-policy configuration.
func URLOriginInList(target *url.URL, origins string) bool {
	if target == nil || target.Scheme == "" || target.Host == "" {
		return false
	}
	origin := target.Scheme + "://" + target.Host
	for configured := range strings.SplitSeq(origins, ",") {
		if strings.TrimSpace(configured) == origin {
			return true
		}
	}
	return false
}
