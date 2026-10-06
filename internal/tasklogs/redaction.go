package tasklogs

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"sync"
)

type redactor struct {
	mu       sync.RWMutex
	secrets  []string
	bytes    int
	overflow bool
}

func (r *redactor) add(value string) {
	if value == "" {
		return
	}
	if len(value) > 1024*1024 {
		r.mu.Lock()
		r.overflow = true
		r.mu.Unlock()
		return
	}
	variants := []string{value, url.QueryEscape(value)}
	encoded, _ := json.Marshal(value)
	variants = append(variants, string(encoded[1:len(encoded)-1]))
	variants = append(variants, strings.FieldsFunc(value, func(c rune) bool { return c == '\n' || c == '\r' })...)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, secret := range variants {
		if secret == "" {
			continue
		}
		found := false
		for _, existing := range r.secrets {
			if existing == secret {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if len(r.secrets) >= 4096 || r.bytes+len(secret) > 1024*1024 {
			r.overflow = true
			return
		}
		r.bytes += len(secret)
		r.secrets = append(r.secrets, secret)
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

func (r *redactor) redact(message string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.overflow {
		return "[REDACTED]"
	}
	for _, secret := range r.secrets {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}
