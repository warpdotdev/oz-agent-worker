package tasklogs

import (
	"encoding/json"
	"errors"
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
	replacer *strings.Replacer
}

type redactionBuffer struct {
	strings.Builder
}

func (b *redactionBuffer) Write(p []byte) (int, error) {
	return b.WriteString(string(p))
}

func (b *redactionBuffer) WriteString(s string) (int, error) {
	if len(s) > maxRecordBytes-b.Len() {
		return 0, errors.New("redacted log exceeds record budget")
	}
	return b.Builder.WriteString(s)
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
	var pairs []string
	for _, secret := range r.secrets {
		pairs = append(pairs, secret, "[REDACTED]")
	}
	r.replacer = strings.NewReplacer(pairs...)
}

func (r *redactor) redact(message string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.overflow || len(message) > maxLineBytes {
		return "[REDACTED]"
	}
	var output redactionBuffer
	if r.replacer == nil {
		if _, err := output.WriteString(message); err != nil {
			return "[REDACTED]"
		}
	} else if _, err := r.replacer.WriteString(&output, message); err != nil {
		return "[REDACTED]"
	}
	return output.String()
}
