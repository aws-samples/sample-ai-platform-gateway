// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package embedstore implements ports.ContentStore by reading the help content
// EMBEDDED in the binary (go:embed). Content is immutable within the process, so
// it is loaded once and cached. Updating content = edit the .md files + redeploy
// help-api (it does not touch the Console).
//
// Layout: content/<kind>/<lang>/<file>, with <lang> from manifest.languages.
// English is the source language, so it is the first link of the fallback chain.
package embedstore

import (
	"context"
	"embed"
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/aiplat/help/internal/help"
	"github.com/aiplat/help/internal/ports"
)

// Embedding the tree recursively rather than one glob per language folder:
// a per-language glob (content/faq/es/*.md) is a COMPILE error while that
// language has no file yet, which would make "translation in progress" an
// unbuildable state. A missing file is already handled and logged at load time,
// so the recursive form costs nothing and keeps every intermediate state green.
//
//go:embed content
var fs embed.FS

// manifestEntry is the record of one item in manifest.json. Title is a per-language
// map because the deep-dive list shows the title, and a single title would leave a
// Portuguese heading above English prose.
type manifestEntry struct {
	File    string            `json:"file"`
	Title   map[string]string `json:"title,omitempty"`
	Version int               `json:"version"`
}

type manifestDoc struct {
	ContractVersion string                   `json:"contract_version"`
	Languages       []string                 `json:"languages"`
	FAQ             map[string]manifestEntry `json:"faq"`
	Internal        map[string]manifestEntry `json:"internal"`
}

// Store loads the bundle from the embedded FS, with caching.
type Store struct {
	once sync.Once
	b    help.Bundle
	err  error
}

var _ ports.ContentStore = (*Store)(nil)

func New() *Store { return &Store{} }

func (s *Store) Load(ctx context.Context) (help.Bundle, error) {
	s.once.Do(func() { s.b, s.err = build() })
	return s.b, s.err
}

// build reads the manifest and assembles one Catalog per language, OMITTING
// malformed items (missing file or empty body) with a structured log — one bad
// item must not take down the rest, and a language with no file at all is a
// normal state while translation is in progress.
func build() (help.Bundle, error) {
	raw, err := fs.ReadFile("content/manifest.json")
	if err != nil {
		return help.Bundle{}, err
	}
	var m manifestDoc
	if err := json.Unmarshal(raw, &m); err != nil {
		return help.Bundle{}, err
	}

	chain := m.Languages
	if len(chain) == 0 {
		chain = []string{help.DefaultLang}
	}

	b := help.Bundle{
		ContractVersion: m.ContractVersion,
		Chain:           chain,
		Cat:             map[string]help.Catalog{},
	}

	// missing counts absent files per language so the log line is one summary
	// instead of one warning per file — 21 items × 3 languages would be noise.
	missing := map[string]int{}

	load := func(kind, lang, key string, e manifestEntry, dst map[string]help.Item) {
		path := "content/" + kind + "/" + lang + "/" + e.File
		raw, err := fs.ReadFile(path)
		if err != nil {
			missing[lang]++
			return
		}
		body := strings.TrimSpace(string(raw))
		if body == "" {
			missing[lang]++
			return
		}
		dst[key] = help.Item{Key: key, Title: e.Title[lang], Body: body, Version: e.Version}
	}

	for _, lang := range chain {
		cat := help.Catalog{
			ContractVersion: m.ContractVersion,
			FAQ:             map[string]help.Item{},
			Internal:        map[string]help.Item{},
		}
		for k, e := range m.FAQ {
			load("faq", lang, k, e, cat.FAQ)
		}
		for k, e := range m.Internal {
			load("internal", lang, k, e, cat.Internal)
		}
		b.Cat[lang] = cat
	}

	for _, lang := range chain {
		if n := missing[lang]; n > 0 {
			log.Printf(`{"lvl":"info","msg":"help content incomplete","lang":%q,"missing":%d}`, lang, n)
		}
	}
	return b, nil
}
