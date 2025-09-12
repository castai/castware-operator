package controller

import (
	"errors"
	"strings"
)

func unflattenMap(m map[string]string) (map[string]interface{}, error) {
	var ok bool
	values := map[string]interface{}{}
	for path, value := range m {
		keys := strings.Split(path, ".")
		selectedMap := values
		for _, key := range keys[:len(keys)-1] {
			if _, ok := selectedMap[key]; !ok {
				selectedMap[key] = map[string]interface{}{}
			}
			selectedMap, ok = selectedMap[key].(map[string]interface{})
			if !ok {
				return nil, errors.New("invalid map")
			}
		}
		selectedMap[keys[len(keys)-1]] = value
	}
	return values, nil
}
