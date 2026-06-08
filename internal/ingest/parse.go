package ingest

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/readeck/go-readability"
)

// DeriveSourceName returns the canonical citation name and source type for a location.
// URLs return (full URL, "url"); .txt files return (basename, "text"); other file paths return (basename, "pdf").
func DeriveSourceName(location string) (string, string, error) {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		if _, err := url.Parse(location); err != nil {
			return "", "", fmt.Errorf("invalid URL: %w", err)
		}
		return location, "url", nil
	}
	base := filepath.Base(location)
	if strings.EqualFold(filepath.Ext(base), ".txt") {
		return base, "text", nil
	}
	return base, "pdf", nil
}

// ExtractURL fetches a web page and returns its main text content.
func ExtractURL(location string) (string, error) {
	article, err := readability.FromURL(location, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("fetch url %s: %w", location, err)
	}
	return article.TextContent, nil
}

// DefaultURLExtractor implements URLExtractor using go-readability.
type DefaultURLExtractor struct{}

// ExtractURL implements URLExtractor.
func (DefaultURLExtractor) ExtractURL(_ context.Context, location string) (string, error) {
	return ExtractURL(location)
}
