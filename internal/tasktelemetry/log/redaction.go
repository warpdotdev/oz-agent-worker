package tasklogs

import (
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// redactor shares bounded credential patterns between structured events and raw lines.
// Only raw lines match newline fragments, since line splitting removes complete values.
type redactor struct {
	mu           sync.RWMutex
	secrets      []redactionPattern
	bytes        int
	overflow     bool
	replacer     *strings.Replacer
	lineReplacer *strings.Replacer
}

// redactionPattern limits fragment matching to streams split at newline boundaries.
type redactionPattern struct {
	value   string
	rawOnly bool
}

// redactionBuffer caps replacement output before it can exceed the record budget.
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
	variants := []redactionPattern{{value: value}, {value: url.QueryEscape(value)}}
	encoded, _ := json.Marshal(value)
	variants = append(variants, redactionPattern{value: string(encoded[1 : len(encoded)-1])})
	for _, fragment := range strings.FieldsFunc(value, func(c rune) bool { return c == '\n' || c == '\r' }) {
		variants = append(variants, redactionPattern{value: fragment, rawOnly: true})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, secret := range variants {
		if secret.value == "" {
			continue
		}
		found := false
		for i, existing := range r.secrets {
			if existing.value == secret.value {
				r.secrets[i].rawOnly = existing.rawOnly && secret.rawOnly
				found = true
				break
			}
		}
		if found {
			continue
		}
		if len(r.secrets) >= 4096 || r.bytes+len(secret.value) > 1024*1024 {
			r.overflow = true
			return
		}
		r.bytes += len(secret.value)
		r.secrets = append(r.secrets, secret)
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i].value) > len(r.secrets[j].value) })
	var pairs, linePairs []string
	for _, secret := range r.secrets {
		linePairs = append(linePairs, secret.value, "[REDACTED]")
		if !secret.rawOnly {
			pairs = append(pairs, secret.value, "[REDACTED]")
		}
	}
	r.replacer = strings.NewReplacer(pairs...)
	r.lineReplacer = strings.NewReplacer(linePairs...)
}

func (r *redactor) redact(message string) string {
	return r.redactWith(message, false)
}

func (r *redactor) redactLine(message string) string {
	return r.redactWith(message, true)
}

func (r *redactor) redactWith(message string, rawLine bool) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.overflow || len(message) > maxLineBytes {
		return "[REDACTED]"
	}
	var output redactionBuffer
	replacer := r.replacer
	if rawLine {
		replacer = r.lineReplacer
	}
	if replacer == nil {
		if _, err := output.WriteString(message); err != nil {
			return "[REDACTED]"
		}
	} else if _, err := replacer.WriteString(&output, message); err != nil {
		return "[REDACTED]"
	}
	return output.String()
}
