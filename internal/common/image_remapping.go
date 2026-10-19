package common

import (
	// Register the OCI digest algorithms used by reference parsing.
	_ "crypto/sha256"
	_ "crypto/sha512"
	"fmt"
	"sort"
	"strings"

	"github.com/distribution/reference"
)

// ImageRemapper matches normalized repositories, independently of tags and digests.
type ImageRemapper struct {
	repositories map[string]reference.Named
}

func NewImageRemapper(mappings map[string]string) (*ImageRemapper, error) {
	remapper := &ImageRemapper{repositories: make(map[string]reference.Named, len(mappings))}
	sources := make([]string, 0, len(mappings))
	for source := range mappings {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		from, err := reference.ParseNormalizedNamed(source)
		if err != nil {
			return nil, fmt.Errorf("invalid source repository %q: %w", source, err)
		}
		if !reference.IsNameOnly(from) {
			return nil, fmt.Errorf("source repository %q must not include a tag or digest", source)
		}
		if _, exists := remapper.repositories[from.String()]; exists {
			return nil, fmt.Errorf("duplicate source repository %q after normalization", source)
		}
		to, err := reference.ParseNormalizedNamed(mappings[source])
		if err != nil {
			return nil, fmt.Errorf("invalid destination for repository %q: %w", source, err)
		}
		remapper.repositories[from.String()] = to
	}
	return remapper, nil
}

// Remap applies one mapping. A pinned destination replaces the source version;
// otherwise the source's tag and digest are carried over to the new repository.
func (r *ImageRemapper) Remap(image string) string {
	if r == nil || len(r.repositories) == 0 {
		return image
	}
	from, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return image
	}
	repository := reference.TrimNamed(from).String()
	to, ok := r.repositories[repository]
	if !ok {
		return image
	}
	if !reference.IsNameOnly(to) {
		return to.String()
	}
	return to.String() + strings.TrimPrefix(from.String(), repository)
}
