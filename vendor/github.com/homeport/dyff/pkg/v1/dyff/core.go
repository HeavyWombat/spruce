// Copyright © 2019 The Homeport Team
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package dyff

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/homeport/gonvenience/pkg/v1/bunt"
	"github.com/homeport/gonvenience/pkg/v1/text"
	"github.com/homeport/ytbx/pkg/v1/ytbx"
	"github.com/mitchellh/hashstructure"
	"github.com/pkg/errors"
	"github.com/texttheater/golang-levenshtein/levenshtein"
	yaml "gopkg.in/yaml.v2"
)

// NonStandardIdentifierGuessCountThreshold specifies how many list entries are
// needed for the guess-the-identifier function to actually consider the key
// name. Or in short, if the lists only contain two entries each, there are more
// possibilities to find unique enough keys, which might no qualify as such.
var NonStandardIdentifierGuessCountThreshold = 3

// MinorChangeThreshold specifies how many percent of the text needs to be
// changed so that it still qualifies as being a minor string change.
var MinorChangeThreshold = 0.1

// UseGoPatchPaths style paths instead of Spruce Dot-Style
var UseGoPatchPaths = false

// bold returns the provided string in 'bold' format
func bold(text string) string {
	return bunt.Style(text, bunt.Bold)
}

// italic returns the provided string in 'italic' format
func italic(text string) string {
	return bunt.Style(text, bunt.Italic)
}

func green(text string) string {
	return bunt.Colorize(text, bunt.AdditionGreen)
}

func red(text string) string {
	return bunt.Colorize(text, bunt.RemovalRed)
}

func yellow(text string) string {
	return bunt.Colorize(text, bunt.ModificationYellow)
}

// CompareInputFiles is one of the convenience main entry points for comparing objects. In this case the representation of an input file, which might contain multiple documents. It returns a report with the list of differences. Each difference describes a change to comes from "from" to "to", hence the names.
func CompareInputFiles(from ytbx.InputFile, to ytbx.InputFile) (Report, error) {
	if len(from.Documents) != len(to.Documents) {
		return Report{}, fmt.Errorf("comparing YAMLs with a different number of documents is currently not supported")
	}

	result := make([]Diff, 0)
	for idx := range from.Documents {
		diffs, err := compareObjects(ytbx.Path{DocumentIdx: idx}, from.Documents[idx], to.Documents[idx])
		if err != nil {
			return Report{}, err
		}

		result = append(result, diffs...)
	}

	return Report{from, to, result}, nil
}

func compareObjects(path ytbx.Path, from interface{}, to interface{}) ([]Diff, error) {
	// Save some time and process some simple nil and type-change use cases immediately
	if (from == nil && to != nil) || (from != nil && to == nil) {
		return []Diff{{path, []Detail{{Kind: MODIFICATION, From: from, To: to}}}}, nil

	} else if from == nil && to == nil {
		return []Diff{}, nil

	} else if reflect.TypeOf(from) != reflect.TypeOf(to) {
		return []Diff{{path, []Detail{{Kind: MODIFICATION, From: from, To: to}}}}, nil
	}

	var diffs []Diff
	var err error

	switch from.(type) {
	case yaml.MapSlice:
		diffs, err = compareMapSlices(path, from.(yaml.MapSlice), to.(yaml.MapSlice))

	case []interface{}:
		diffs, err = compareLists(path, from.([]interface{}), to.([]interface{}))

	case []yaml.MapSlice:
		diffs, err = compareListOfMapSlices(path, from.([]yaml.MapSlice), to.([]yaml.MapSlice))

	case string:
		diffs, err = compareStrings(path, from.(string), to.(string))

	case bool, float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
		if from != to {
			diffs, err = []Diff{{path, []Detail{{Kind: MODIFICATION, From: from, To: to}}}}, nil
		}

	default:
		err = fmt.Errorf("failed to compare objects due to unsupported type %s", reflect.TypeOf(from))
	}

	return diffs, err
}

func compareMapSlices(path ytbx.Path, from yaml.MapSlice, to yaml.MapSlice) ([]Diff, error) {
	removals := yaml.MapSlice{}
	additions := yaml.MapSlice{}

	result := make([]Diff, 0)

	for _, fromItem := range from {
		key := fromItem.Key
		if toItem, ok := getMapItemByKeyFromMapSlice(key, to); ok {
			// `from` and `to` contain the same `key` -> require comparison
			diffs, err := compareObjects(ytbx.NewPathWithNamedElement(path, key), fromItem.Value, toItem.Value)
			if err != nil {
				return nil, err
			}
			result = append(result, diffs...)

		} else {
			// `from` contain the `key`, but `to` does not -> removal
			removals = append(removals, fromItem)
		}
	}

	for _, toItem := range to {
		key := toItem.Key
		if _, ok := getMapItemByKeyFromMapSlice(key, from); !ok {
			// `to` contains a `key` that `from` does not have -> addition
			additions = append(additions, toItem)
		}
	}

	diff := Diff{Path: path, Details: []Detail{}}

	if len(removals) > 0 {
		diff.Details = append(diff.Details, Detail{Kind: REMOVAL, From: removals, To: nil})
	}

	if len(additions) > 0 {
		diff.Details = append(diff.Details, Detail{Kind: ADDITION, From: nil, To: additions})
	}

	if len(diff.Details) > 0 {
		result = append([]Diff{diff}, result...)
	}

	return result, nil
}

func compareLists(path ytbx.Path, from []interface{}, to []interface{}) ([]Diff, error) {
	// Bail out quickly if there is nothing to check
	if len(from) == 0 && len(to) == 0 {
		return []Diff{}, nil
	}

	if identifier, err := getIdentifierFromNamedLists(from, to); err == nil {
		return compareNamedEntryLists(path, identifier, from, to)
	}

	if identifier := getNonStandardIdentifierFromNamedLists(from, to); identifier != "" {
		return compareNamedEntryLists(path, identifier, from, to)
	}

	return compareSimpleLists(path, from, to)
}

func compareListOfMapSlices(path ytbx.Path, from []yaml.MapSlice, to []yaml.MapSlice) ([]Diff, error) {
	// TODO Check if there is another way to do this, or if we can save time by doing something else
	return compareLists(path, ytbx.SimplifyList(from), ytbx.SimplifyList(to))
}

func compareSimpleLists(path ytbx.Path, from []interface{}, to []interface{}) ([]Diff, error) {
	removals := make([]interface{}, 0)
	additions := make([]interface{}, 0)

	result := make([]Diff, 0)

	fromLength := len(from)
	toLength := len(to)

	// Special case if both lists only contain one entry: directly compare the two entries with each other
	if fromLength == 1 && fromLength == toLength {
		return compareObjects(ytbx.NewPathWithIndexedListElement(path, 0), from[0], to[0])
	}

	fromLookup, err := createLookUpMap(from)
	if err != nil {
		return nil, err
	}

	toLookup, err := createLookUpMap(to)
	if err != nil {
		return nil, err
	}

	// Fill two lists with the names of the entries that are common to both provided lists
	fromNames := make([]uint64, 0, fromLength)
	toNames := make([]uint64, 0, fromLength)

	for idxPos, fromValue := range from {
		hash, err := calcHash(fromValue)
		if err != nil {
			return nil, err
		}

		if _, ok := toLookup[hash]; !ok {
			// `from` entry does not exist in `to` list
			removals = append(removals, from[idxPos])

		} else {
			fromNames = append(fromNames, hash)
		}
	}

	for idxPos, toValue := range to {
		hash, err := calcHash(toValue)
		if err != nil {
			return nil, err
		}

		if _, ok := fromLookup[hash]; !ok {
			// `to` entry does not exist in `from` list
			additions = append(additions, to[idxPos])

		} else {
			toNames = append(toNames, hash)
		}
	}

	orderchanges := findOrderChangesInSimpleList(from, to, fromNames, toNames, fromLookup, toLookup)

	return packChangesAndAddToResult(result, true, path, orderchanges, additions, removals)
}

func compareNamedEntryLists(path ytbx.Path, identifier string, from []interface{}, to []interface{}) ([]Diff, error) {
	removals := make([]interface{}, 0)
	additions := make([]interface{}, 0)

	result := make([]Diff, 0)

	// Fill two lists with the names of the entries that are common to both provided lists
	fromLength := len(from)
	fromNames := make([]string, 0, fromLength)
	toNames := make([]string, 0, fromLength)

	// Find entries that are common to both lists to compare them separately, and find entries that are only in from, but not to and are therefore removed
	for _, fromEntry := range from {
		name, err := getValueByKey(fromEntry.(yaml.MapSlice), identifier)
		if err != nil {
			return nil, err
		}

		if toEntry, ok := getEntryFromNamedList(to, identifier, name); ok {
			// `from` and `to` have the same entry idenfified by identifier and name -> require comparison
			diffs, err := compareObjects(ytbx.NewPathWithNamedListElement(path, identifier, name), fromEntry, toEntry)
			if err != nil {
				return nil, err
			}
			result = append(result, diffs...)
			fromNames = append(fromNames, name.(string))

		} else {
			// `from` has an entry (identified by identifier and name), but `to` does not -> removal
			removals = append(removals, fromEntry)
		}
	}

	// Find entries that are only in to, but not from and are therefore added
	for _, toEntry := range to {
		name, err := getValueByKey(toEntry.(yaml.MapSlice), identifier)
		if err != nil {
			return nil, err
		}

		if _, ok := getEntryFromNamedList(from, identifier, name); ok {
			// `to` and `from` have the same entry idenfified by identifier and name (comparison already covered by previous range)
			toNames = append(toNames, name.(string))

		} else {
			// `to` has an entry (identified by identifier and name), but `from` does not -> addition
			additions = append(additions, toEntry)
		}
	}

	orderchanges := findOrderChangesInNamedEntryLists(fromNames, toNames)

	return packChangesAndAddToResult(result, true, path, orderchanges, additions, removals)
}

func compareStrings(path ytbx.Path, from string, to string) ([]Diff, error) {
	result := make([]Diff, 0)
	if strings.Compare(from, to) != 0 {
		result = append(result, Diff{path, []Detail{{Kind: MODIFICATION, From: from, To: to}}})
	}

	return result, nil
}

func findOrderChangesInSimpleList(from, to []interface{}, fromNames, toNames []uint64, fromLookup, toLookup map[uint64]int) []Detail {
	orderchanges := make([]Detail, 0)

	// Try to find order changes ...
	if len(fromNames) == len(toNames) {
		for idx, hash := range fromNames {
			if toNames[idx] != hash {
				cnv := func(list []uint64, lookup map[uint64]int, content []interface{}) []interface{} {
					result := make([]interface{}, 0, len(list))
					for _, hash := range list {
						result = append(result, content[lookup[hash]])
					}

					return result
				}

				orderchanges = append(orderchanges,
					Detail{
						Kind: ORDERCHANGE,
						From: cnv(fromNames, fromLookup, from),
						To:   cnv(toNames, toLookup, to),
					})
				break
			}
		}
	}

	return orderchanges
}

func findOrderChangesInNamedEntryLists(fromNames, toNames []string) []Detail {
	orderchanges := make([]Detail, 0)

	// Try to find order changes ...
	idxLookupMap := make(map[string]int, len(toNames))
	for idx, name := range toNames {
		idxLookupMap[name] = idx
	}

	for idx, name := range fromNames {
		if idxLookupMap[name] != idx {
			orderchanges = append(orderchanges, Detail{Kind: ORDERCHANGE, From: fromNames, To: toNames})
			break
		}
	}

	return orderchanges
}

func packChangesAndAddToResult(list []Diff, prepend bool, path ytbx.Path, orderchanges []Detail, additions, removals []interface{}) ([]Diff, error) {
	// Prepare a diff for this path to added to the result set (if there are changes)
	diff := Diff{Path: path, Details: []Detail{}}

	if len(orderchanges) > 0 {
		diff.Details = append(diff.Details, orderchanges...)
	}

	if len(removals) > 0 {
		diff.Details = append(diff.Details, Detail{Kind: REMOVAL, From: removals, To: nil})
	}

	if len(additions) > 0 {
		diff.Details = append(diff.Details, Detail{Kind: ADDITION, From: nil, To: additions})
	}

	// If there were changes added to the details list,
	// we can safely add it to the result set.
	// Otherwise it the result set will be returned as-is.
	if len(diff.Details) > 0 {
		switch prepend {
		case true:
			list = append([]Diff{diff}, list...)

		case false:
			list = append(list, diff)
		}
	}

	return list, nil
}

// getMapItemByKeyFromMapSlice returns the MapItem (tuple of key/value) where the MapItem key matches the provided key. It will return an empty MapItem and bool false if the given MapSlice does not contain a suitable MapItem.
func getMapItemByKeyFromMapSlice(key interface{}, mapslice yaml.MapSlice) (yaml.MapItem, bool) {
	for _, mapitem := range mapslice {
		if mapitem.Key == key {
			return mapitem, true
		}
	}

	return yaml.MapItem{}, false
}

// getValueByKey returns the value for a given key in a provided MapSlice, or nil with an error if there is no such entry. This is comparable to getting a value from a map with `foobar[key]`.
func getValueByKey(mapslice yaml.MapSlice, key string) (interface{}, error) {
	for _, element := range mapslice {
		if element.Key == key {
			return element.Value, nil
		}
	}

	if names, err := ytbx.ListStringKeys(mapslice); err == nil {
		return nil, fmt.Errorf("no key '%s' found in map, available keys are: %s", key, strings.Join(names, ", "))
	}

	return nil, fmt.Errorf("no key '%s' found in map and also failed to get a list of key for this map", key)
}

// getEntryFromNamedList returns the entry that is identified by the identifier key and a name, for example: `name: one` where name is the identifier key and one the name. Function will return nil with bool false if there is no such entry.
func getEntryFromNamedList(list []interface{}, identifier string, name interface{}) (interface{}, bool) {
	for _, listEntry := range list {
		mapslice := listEntry.(yaml.MapSlice)

		for _, element := range mapslice {
			if element.Key == identifier && element.Value == name {
				return mapslice, true
			}
		}
	}

	return nil, false
}

// GetIdentifierFromNamedList returns the identifier key used in the provided list, or an empty string if there is none. The identifier key is either 'name', 'key', or 'id'.
func GetIdentifierFromNamedList(list []interface{}) string {
	counters := map[interface{}]int{}

	for _, sliceEntry := range list {
		switch mapslice := sliceEntry.(type) {
		case yaml.MapSlice:
			for _, mapSliceEntry := range mapslice {
				if _, ok := counters[mapSliceEntry.Key]; !ok {
					counters[mapSliceEntry.Key] = 0
				}

				counters[mapSliceEntry.Key]++
			}
		}
	}

	sliceLength := len(list)
	for _, identifier := range []string{"name", "key", "id"} {
		if count, ok := counters[identifier]; ok && count == sliceLength {
			return identifier
		}
	}

	return ""
}

func getIdentifierFromNamedLists(listA, listB []interface{}) (string, error) {
	createKeyCountMap := func(list []interface{}) (map[interface{}]map[uint64]struct{}, error) {
		result := map[interface{}]map[uint64]struct{}{}
		for _, entry := range list {
			switch mapslice := entry.(type) {
			case yaml.MapSlice:
				for _, mapitem := range mapslice {
					hash, err := calcHash(mapitem.Value)
					if err != nil {
						return nil, err
					}

					if _, found := result[mapitem.Key]; !found {
						result[mapitem.Key] = map[uint64]struct{}{}
					}

					result[mapitem.Key][hash] = struct{}{}
				}
			}
		}

		return result, nil
	}

	counterA, err := createKeyCountMap(listA)
	if err != nil {
		return "", err
	}

	counterB, err := createKeyCountMap(listB)
	if err != nil {
		return "", err
	}

	// Check for the usual suspects: name, key, and id
	for _, identifier := range []string{"name", "key", "id"} {
		if countA, okA := counterA[identifier]; okA && len(countA) == len(listA) {
			if countB, okB := counterB[identifier]; okB && len(countB) == len(listB) {
				return identifier, nil
			}
		}
	}

	return "", fmt.Errorf("unable to find a key that can serve as an unique identifier")
}

func getNonStandardIdentifierFromNamedLists(listA, listB []interface{}) string {
	createKeyCountMap := func(list []interface{}) map[string]int {
		tmp := map[string]map[string]struct{}{}
		for _, entry := range list {
			switch entry.(type) {
			case yaml.MapSlice:
				for _, mapitem := range entry.(yaml.MapSlice) {
					switch mapitem.Key.(type) {
					case string:
						key := mapitem.Key.(string)
						switch mapitem.Value.(type) {
						case string:
							if _, ok := tmp[key]; !ok {
								tmp[key] = map[string]struct{}{}
							}

							tmp[key][mapitem.Value.(string)] = struct{}{}
						}
					}
				}
			}
		}

		result := map[string]int{}
		for key, value := range tmp {
			result[key] = len(value)
		}

		return result
	}

	listALength := len(listA)
	listBLength := len(listB)
	counterA := createKeyCountMap(listA)
	counterB := createKeyCountMap(listB)

	for keyA, countA := range counterA {
		if countB, ok := counterB[keyA]; ok {
			if countA == listALength && countB == listBLength && countA > NonStandardIdentifierGuessCountThreshold {
				return keyA
			}
		}
	}

	return ""
}

func createLookUpMap(list []interface{}) (map[uint64]int, error) {
	result := make(map[uint64]int, len(list))
	for idx, entry := range list {
		hash, err := calcHash(entry)
		if err != nil {
			return nil, err
		}
		result[hash] = idx
	}

	return result, nil
}

func calcHash(obj interface{}) (uint64, error) {
	var hash uint64
	var err error

	// Convert YAML MapSlices to maps first so that the order of keys does not matter for the hash value of this object
	switch mapslice := obj.(type) {
	case yaml.MapSlice:
		tmp := make(map[interface{}]interface{}, len(mapslice))
		for _, entry := range mapslice {
			tmp[entry.Key] = entry.Value
		}
		obj = tmp
	}

	if hash, err = hashstructure.Hash(obj, nil); err != nil {
		return 0, errors.Wrap(err, "Failed to calculate hash")
	}

	return hash, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}

	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}

	return b
}

func isMinorChange(from string, to string) bool {
	levenshteinDistance := levenshtein.DistanceForStrings([]rune(from), []rune(to), levenshtein.DefaultOptions)

	// Special case: Consider it a minor change if only two runes/characters were
	// changed, which results in a default distance of four, two removals and two
	// additions each.
	if levenshteinDistance <= 4 {
		return true
	}

	referenceLength := min(len(from), len(to))
	return float64(levenshteinDistance)/float64(referenceLength) < MinorChangeThreshold
}

func isMultiLine(from string, to string) bool {
	return strings.Contains(from, "\n") || strings.Contains(to, "\n")
}

func isValidUTF8String(from string, to string) bool {
	return utf8.Valid([]byte(from)) || utf8.Valid([]byte(to))
}

func isList(obj interface{}) bool {
	switch obj.(type) {
	case []interface{}:
		return true

	default:
		return false
	}
}

// ChangeRoot changes the root of an input file to a position inside its document based on the given path. Input files with more than one document are not supported, since they could have multiple elements with that path.
func ChangeRoot(inputFile *ytbx.InputFile, path string, translateListToDocuments bool) error {
	multipleDocuments := len(inputFile.Documents) != 1

	if multipleDocuments {
		return fmt.Errorf("change root for an input file is only possible if there is only one document, but %s contains %s",
			inputFile.Location,
			text.Plural(len(inputFile.Documents), "document"))
	}

	// For reference reasons, keep the original root level
	originalRoot := inputFile.Documents[0]

	// Find the object at the given path
	obj, err := ytbx.Grab(inputFile.Documents[0], path)
	if err != nil {
		return err
	}

	if translateListToDocuments && isList(obj) {
		// Change root of input file main document to a new list of documents based on the the list that was found
		inputFile.Documents = obj.([]interface{})

	} else {
		// Change root of input file main document to the object that was found
		inputFile.Documents = []interface{}{obj}
	}

	// Parse path string and create nicely formatted output path
	if resolvedPath, err := ytbx.ParsePathString(path, originalRoot); err == nil {
		path = pathToString(resolvedPath, multipleDocuments)
	}

	inputFile.Note = fmt.Sprintf("YAML root was changed to %s", path)

	return nil
}

func pathToString(path ytbx.Path, showDocumentIdx bool) string {
	var result string

	if UseGoPatchPaths {
		result = styledGoPatchPath(path)

	} else {
		result = styledDotStylePath(path)
	}

	if showDocumentIdx {
		result += bunt.Sprintf("  LightSteelBlue{(document #%d)}", path.DocumentIdx+1)
	}

	return result
}
