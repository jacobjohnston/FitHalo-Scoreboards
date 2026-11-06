package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Languages are FitHalo's ten, in the order the footer lists them.
var Languages = []string{"en", "de", "es", "es-419", "fr", "it", "ja", "ko", "pt-BR", "zh-Hans"}

// defaultLanguage is the language every other table must match, and the last fallback.
const defaultLanguage = "en"

// languageNames are each language in itself, for the footer's menu.
var languageNames = map[string]string{
	"en": "English", "de": "Deutsch", "es": "Español (España)", "es-419": "Español (Latinoamérica)",
	"fr": "Français", "it": "Italiano", "ja": "日本語", "ko": "한국어", "pt-BR": "Português (Brasil)", "zh-Hans": "简体中文",
}

// Localizer holds every language's strings.
type Localizer struct {
	// strings holds each language's table of text, by language code and then key.
	strings map[string]map[string]string
}

// LoadLocalizer reads each language's strings/<language>.json, which must hold exactly English's keys.
func LoadLocalizer(files fs.FS) (*Localizer, error) {
	localizer := &Localizer{strings: map[string]map[string]string{}}
	for _, language := range Languages {
		data, err := fs.ReadFile(files, "strings/"+language+".json")
		if err != nil {
			return nil, err
		}
		table := map[string]string{}
		if err := json.Unmarshal(data, &table); err != nil {
			return nil, fmt.Errorf("web: strings/%s.json: %w", language, err)
		}
		localizer.strings[language] = table
	}
	for key := range localizer.strings[defaultLanguage] {
		for _, language := range Languages {
			if strings.TrimSpace(localizer.strings[language][key]) == "" {
				return nil, fmt.Errorf("web: strings/%s.json has no %s", language, key)
			}
		}
	}
	for _, language := range Languages {
		for key := range localizer.strings[language] {
			if _, known := localizer.strings[defaultLanguage][key]; !known {
				return nil, fmt.Errorf("web: strings/%s.json has %s, which English does not", language, key)
			}
		}
	}
	return localizer, nil
}

// Text returns a string in language with its {placeholders} filled from pairs of names and values.
func (l *Localizer) Text(language, key string, arguments ...any) string {
	text, found := l.strings[language][key]
	if !found {
		text, found = l.strings[defaultLanguage][key]
	}
	if !found {
		return key
	}
	for index := 0; index+1 < len(arguments); index += 2 {
		text = strings.ReplaceAll(text, "{"+fmt.Sprint(arguments[index])+"}", fmt.Sprint(arguments[index+1]))
	}
	return text
}

// Keys returns English's keys, sorted, for tests.
func (l *Localizer) Keys() []string {
	keys := make([]string, 0, len(l.strings[defaultLanguage]))
	for key := range l.strings[defaultLanguage] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// chooseLanguage picks a request's language: the cookie's, then the browser's preferences, then English.
func chooseLanguage(request *http.Request) string {
	if cookie, err := request.Cookie(languageCookie); err == nil {
		for _, language := range Languages {
			if cookie.Value == language {
				return language
			}
		}
	}
	return MatchLanguage(request.Header.Get("Accept-Language"))
}

// MatchLanguage picks FitHalo's best language for an Accept-Language header, mapping regions to variants.
func MatchLanguage(header string) string {
	type preference struct {
		tag     string
		quality float64
		order   int
	}
	var preferences []preference
	for order, part := range strings.Split(header, ",") {
		tag, parameters, _ := strings.Cut(strings.TrimSpace(part), ";")
		quality := 1.0
		if value, found := strings.CutPrefix(strings.TrimSpace(parameters), "q="); found {
			if parsed, err := strconv.ParseFloat(value, 64); err == nil {
				quality = parsed
			}
		}
		if tag != "" && quality > 0 {
			preferences = append(preferences, preference{strings.ToLower(tag), quality, order})
		}
	}
	sort.SliceStable(preferences, func(first, second int) bool { return preferences[first].quality > preferences[second].quality })
	for _, wanted := range preferences {
		if language := matchTag(wanted.tag); language != "" {
			return language
		}
	}
	return defaultLanguage
}

// matchTag returns FitHalo's language for one lowercase tag, or empty when none fits.
func matchTag(tag string) string {
	for _, language := range Languages {
		if strings.EqualFold(tag, language) {
			return language
		}
	}
	base, region, _ := strings.Cut(tag, "-")
	switch base {
	case "zh":
		if strings.HasPrefix(region, "hant") || region == "tw" || region == "hk" || region == "mo" {
			return ""
		}
		return "zh-Hans"
	case "es":
		if region == "" || region == "es" {
			return "es"
		}
		return "es-419"
	case "pt":
		return "pt-BR"
	}
	for _, language := range Languages {
		if base == language {
			return language
		}
	}
	return ""
}
