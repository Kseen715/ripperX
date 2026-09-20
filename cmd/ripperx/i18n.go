package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

// The pages carry no English of their own: every word they show is looked up
// in a locale file, and the files are ordinary JSON served from the same
// place as the pages. Adding a language is therefore adding one file to
// web/locales - nothing here and nothing in the pages has to know about it,
// because this reads the directory rather than a list somebody maintains.
//
// Each file names itself in its own language, under "_name", so the menu is
// built from the files too.

// defaultLanguage is the language a server serves when it was not told
// otherwise, and the last fallback for a string missing everywhere else.
const defaultLanguage = "en"

type language struct {
	Code string `json:"code" doc:"the file's name, and what to pass as the lang parameter"`
	Name string `json:"name" doc:"what the language calls itself"`
}

type localesResponse struct {
	Default   string     `json:"default" doc:"the language this server was configured with; the fallback for anything missing from another"`
	Languages []language `json:"languages" doc:"every language installed, by the name it calls itself"`
}

// readLanguages lists the locale files that came with this binary. A file
// that is not readable JSON is left out rather than fatal: a broken
// translation should cost that language, not the server.
func readLanguages(web fs.FS) []language {
	entries, err := fs.ReadDir(web, "locales")
	if err != nil {
		return nil
	}
	var out []language
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".json")
		b, err := fs.ReadFile(web, path.Join("locales", e.Name()))
		if err != nil {
			continue
		}
		var strs map[string]string
		if json.Unmarshal(b, &strs) != nil {
			continue
		}
		name := strs["_name"]
		if name == "" {
			name = code
		}
		out = append(out, language{Code: code, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// checkLanguage refuses to start on a language that is not installed. A
// server silently serving English because of a typo in its settings file is
// worse than one that will not start and says which languages it has.
func checkLanguage(code string, have []language) error {
	for _, l := range have {
		if l.Code == code {
			return nil
		}
	}
	names := make([]string, 0, len(have))
	for _, l := range have {
		names = append(names, l.Code)
	}
	return fmt.Errorf("lang: %q is not one of the languages this build carries (%s)",
		code, strings.Join(names, ", "))
}

func (s *server) handleLocales(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, localesResponse{Default: s.lang, Languages: s.languages})
}
