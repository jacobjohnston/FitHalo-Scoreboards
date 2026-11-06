package sharing

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Requirement is what one field of a weekly challenge needs, as spec/sharing.json lists it.
type Requirement struct {
	// Score is the least score level the field needs, or empty for none.
	Score ScoreLevel `json:"score,omitempty"`

	// Activity is the least activity level the field needs, or empty for none.
	Activity ActivityLevel `json:"activity,omitempty"`

	// Routes means the field needs route sharing.
	Routes bool `json:"routes,omitempty"`

	// StrengthSets means the field needs strength set sharing.
	StrengthSets bool `json:"strengthSets,omitempty"`

	// StrengthWeights means the field needs strength weight sharing.
	StrengthWeights bool `json:"strengthWeights,omitempty"`
}

// Allowlist is spec/sharing.json: all weekly challenge data and the sharing it needs.
type Allowlist struct {
	// requirements holds each listed path's requirement.
	requirements map[string]Requirement

	// paths is every listed path, sorted.
	paths []string
}

// LoadAllowlist parses spec/sharing.json, and rejects a level or option it does not know.
func LoadAllowlist(data []byte) (*Allowlist, error) {
	var document struct {
		Levels struct {
			Score    []ScoreLevel    `json:"score"`
			Activity []ActivityLevel `json:"activity"`
		} `json:"levels"`
		Fields []struct {
			Path     string                     `json:"path"`
			Requires map[string]json.RawMessage `json:"requires"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("sharing: parsing allowlist: %w", err)
	}
	if !slices.Equal(document.Levels.Score, scoreOrder) || !slices.Equal(document.Levels.Activity, activityOrder) {
		return nil, fmt.Errorf("sharing: the allowlist's levels are not the ones this server knows")
	}
	var ladders struct {
		Levels map[string]json.RawMessage `json:"levels"`
	}
	if err := json.Unmarshal(data, &ladders); err != nil {
		return nil, fmt.Errorf("sharing: parsing allowlist: %w", err)
	}
	for ladder := range ladders.Levels {
		if ladder != "score" && ladder != "activity" {
			return nil, fmt.Errorf("sharing: the allowlist has an unknown ladder %q", ladder)
		}
	}

	allowlist := &Allowlist{requirements: map[string]Requirement{}}
	for _, field := range document.Fields {
		var requirement Requirement
		for key := range field.Requires {
			switch key {
			case "score", "activity", "routes", "strengthSets", "strengthWeights":
			default:
				return nil, fmt.Errorf("sharing: %s requires unknown %q", field.Path, key)
			}
		}
		encoded, _ := json.Marshal(field.Requires)
		if err := json.Unmarshal(encoded, &requirement); err != nil {
			return nil, fmt.Errorf("sharing: %s: %w", field.Path, err)
		}
		if requirement.Score != "" && !slices.Contains(scoreOrder, requirement.Score) {
			return nil, fmt.Errorf("sharing: %s requires unknown score level %q", field.Path, requirement.Score)
		}
		if requirement.Activity != "" && !slices.Contains(activityOrder, requirement.Activity) {
			return nil, fmt.Errorf("sharing: %s requires unknown activity level %q", field.Path, requirement.Activity)
		}
		if _, repeated := allowlist.requirements[field.Path]; repeated {
			return nil, fmt.Errorf("sharing: %s is listed twice", field.Path)
		}
		allowlist.requirements[field.Path] = requirement
		allowlist.paths = append(allowlist.paths, field.Path)
	}
	sort.Strings(allowlist.paths)
	return allowlist, nil
}

// Paths returns every path the allowlist lists, sorted.
func (a *Allowlist) Paths() []string {
	return slices.Clone(a.paths)
}

// Allows reports whether sharing allows the field at path.
func (a *Allowlist) Allows(path string, sharing Sharing) bool {
	normalized := sharing.Normalized()
	if requirement, listed := a.requirements[path]; listed {
		return normalized.meets(requirement)
	}
	// An object or array is allowed when anything beneath it is.
	for _, listed := range a.paths {
		if (strings.HasPrefix(listed, path+".") || strings.HasPrefix(listed, path+"[]")) &&
			normalized.meets(a.requirements[listed]) {
			return true
		}
	}
	return false
}

// meets reports whether s reaches every part of requirement.
func (s Sharing) meets(requirement Requirement) bool {
	if requirement.Score != "" && slices.Index(scoreOrder, s.Score) < slices.Index(scoreOrder, requirement.Score) {
		return false
	}
	if requirement.Activity != "" && slices.Index(activityOrder, s.Activity) < slices.Index(activityOrder, requirement.Activity) {
		return false
	}
	return (!requirement.Routes || s.Routes) &&
		(!requirement.StrengthSets || s.StrengthSets) &&
		(!requirement.StrengthWeights || s.StrengthWeights)
}

// Strip returns a copy of a decoded JSON document with every field sharing does not allow removed. An
// object or array is kept when anything beneath it is allowed, even if it is empty.
func (a *Allowlist) Strip(document map[string]any, sharing Sharing) map[string]any {
	allowed := map[string]bool{}
	for _, path := range a.paths {
		if a.Allows(path, sharing) {
			allowed[path] = true
		}
	}
	stripped, _ := stripValue(document, "", allowed).(map[string]any)
	if stripped == nil {
		stripped = map[string]any{}
	}
	return stripped
}

// stripValue returns value at path with every disallowed leaf removed, or nil when nothing beneath it is
// allowed.
func stripValue(value any, path string, allowed map[string]bool) any {
	switch typed := value.(type) {
	case map[string]any:
		result := map[string]any{}
		for key, child := range typed {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if allowed[childPath] && !isContainer(child) {
				result[key] = child
			} else if containerAllowed(childPath, allowed) {
				if kept := stripValue(child, childPath, allowed); kept != nil {
					result[key] = kept
				}
			}
		}
		return result
	case []any:
		elementPath := path + "[]"
		result := []any{}
		for _, element := range typed {
			if allowed[elementPath] && !isContainer(element) {
				result = append(result, element)
			} else if kept := stripValue(element, elementPath, allowed); kept != nil {
				result = append(result, kept)
			}
		}
		return result
	default:
		return nil
	}
}

// containerAllowed reports whether any allowed path lies beneath path.
// isContainer reports whether a decoded JSON value is an object or an array, which a listed leaf never is.
func isContainer(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func containerAllowed(path string, allowed map[string]bool) bool {
	for allowedPath := range allowed {
		if strings.HasPrefix(allowedPath, path+".") || strings.HasPrefix(allowedPath, path+"[]") {
			return true
		}
	}
	return false
}

// LeafPaths returns the path of every value in a decoded JSON document that is neither an object nor an
// array, in the allowlist's notation, for the leak test.
func LeafPaths(document map[string]any) []string {
	seen := map[string]bool{}
	collectLeaves(document, "", seen)
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// collectLeaves adds the path of every leaf beneath value to seen.
func collectLeaves(value any, path string, seen map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			collectLeaves(child, childPath, seen)
		}
	case []any:
		for _, element := range typed {
			collectLeaves(element, path+"[]", seen)
		}
	default:
		seen[path] = true
	}
}
