package resources

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed strings.json
var files embed.FS

var values = load()

func load() map[string]string {
	data, err := files.ReadFile("strings.json")
	if err != nil {
		panic(err)
	}
	result := map[string]string{}
	if err := json.Unmarshal(data, &result); err != nil {
		panic(err)
	}
	return result
}

func Get(key string) string {
	value, ok := values[key]
	if !ok {
		panic("missing string resource: " + key)
	}
	return value
}

func Format(key string, fields map[string]any) string {
	value := Get(key)
	for name, replacement := range fields {
		value = strings.ReplaceAll(value, "{"+name+"}", fmt.Sprint(replacement))
	}
	return value
}
