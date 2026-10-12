package runtimes

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CheckNode checks every declared engine range against an exact stable Node
// version. It implements npm's numeric comparators, partial/X ranges, caret,
// tilde, hyphen ranges, whitespace intersection, and || union. Prereleases,
// metadata, and other range syntax are rejected rather than loosely interpreted.
// This profile deliberately differs from Composer's version constraints.
func CheckNode(version string, constraints []string) error {
	current, err := parseNodePartial(strings.TrimSpace(version))
	if err != nil || current.parts != 3 {
		return fmt.Errorf("Node version %q must be an exact stable x.y.z version", version)
	}
	// Parse all branches and declarations before choosing a matching branch;
	// an unsupported branch must not be hidden by an earlier successful match.
	ranges := make([][][]nodeComparator, len(constraints))
	for i, constraint := range constraints {
		ranges[i], err = parseNodeRange(constraint)
		if err != nil {
			return fmt.Errorf("unsupported Node engine constraint %q: %w", constraint, err)
		}
	}
	for i, alternatives := range ranges {
		matches := false
		for _, comparators := range alternatives {
			all := true
			for _, comparator := range comparators {
				if !comparator.matches(current.value) {
					all = false
				}
			}
			matches = matches || all
		}
		if !matches {
			return fmt.Errorf("Node %s does not satisfy engine constraint %q", version, constraints[i])
		}
	}
	return nil
}

// npm SemVer bounds each component by Number.MAX_SAFE_INTEGER. Keeping the
// same limit also makes boundary increment checks explicit and deterministic.
const nodeMaxComponent uint64 = 9007199254740991
const nodeMaxRangeBytes = 4096

type nodeVersion [3]uint64

type nodePartial struct {
	value nodeVersion
	parts int // Number of numeric components before a missing or wildcard part.
}

type nodeComparator struct {
	operator string
	value    nodeVersion
}

func (c nodeComparator) matches(current nodeVersion) bool {
	if c.operator == "never" {
		return false
	}
	comparison := 0
	for i := range current {
		if current[i] < c.value[i] {
			comparison = -1
			break
		}
		if current[i] > c.value[i] {
			comparison = 1
			break
		}
	}
	switch c.operator {
	case "=":
		return comparison == 0
	case ">":
		return comparison > 0
	case ">=":
		return comparison >= 0
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	}
	return false
}

func parseNodePartial(text string) (nodePartial, error) {
	var result nodePartial
	text = strings.TrimPrefix(text, "v")
	components := strings.Split(text, ".")
	if text == "" || len(components) > 3 {
		return result, errors.New("expected one to three numeric or wildcard components")
	}
	wildcard := false
	for i, component := range components {
		if component == "*" || component == "x" || component == "X" {
			wildcard = true
			continue
		}
		if wildcard || component == "" || (len(component) > 1 && component[0] == '0') {
			return result, errors.New("invalid numeric or wildcard component")
		}
		for _, character := range component {
			if character < '0' || character > '9' {
				return result, errors.New("only stable numeric versions are supported")
			}
		}
		value, err := strconv.ParseUint(component, 10, 64)
		if err != nil || value > nodeMaxComponent {
			return result, errors.New("version component exceeds npm's safe integer bound")
		}
		result.value[i] = value
		result.parts++
	}
	return result, nil
}

func parseNodeRange(text string) ([][]nodeComparator, error) {
	if len(text) > nodeMaxRangeBytes {
		return nil, errors.New("range exceeds 4096 bytes")
	}
	branches := strings.Split(text, "||")
	result := make([][]nodeComparator, 0, len(branches))
	for _, branch := range branches {
		tokens := strings.Fields(branch)
		comparators := []nodeComparator{}
		for i := 0; i < len(tokens); {
			if i+1 < len(tokens) && tokens[i+1] == "-" {
				if i != 0 || len(tokens) != 3 {
					return nil, errors.New("a hyphen range must be the whole comparator set")
				}
				if i+2 >= len(tokens) {
					return nil, errors.New("hyphen range is missing its upper endpoint")
				}
				from, err := parseNodePartial(tokens[i])
				if err != nil {
					return nil, err
				}
				to, err := parseNodePartial(tokens[i+2])
				if err != nil {
					return nil, err
				}
				if from.parts > 0 {
					comparators = append(comparators, nodeComparator{">=", from.value})
				}
				if to.parts == 3 {
					comparators = append(comparators, nodeComparator{"<=", to.value})
				} else if to.parts > 0 {
					upper, err := nodeNext(to.value, to.parts-1)
					if err != nil {
						return nil, err
					}
					comparators = append(comparators, nodeComparator{"<", upper})
				}
				i += 3
				continue
			}
			token := tokens[i]
			i++
			if isNodeOperator(token) {
				if i >= len(tokens) {
					return nil, errors.New("operator is missing a version")
				}
				token += tokens[i]
				i++
			}
			expanded, err := expandNodeComparator(token)
			if err != nil {
				return nil, err
			}
			comparators = append(comparators, expanded...)
		}
		// npm treats an empty range (including an empty OR branch) as *.
		result = append(result, comparators)
	}
	return result, nil
}

func isNodeOperator(text string) bool {
	switch text {
	case "<", "<=", ">", ">=", "=", "^", "~", "~>":
		return true
	}
	return false
}

func expandNodeComparator(text string) ([]nodeComparator, error) {
	operator := ""
	for _, prefix := range []string{"<=", ">=", "~>", "<", ">", "=", "^", "~"} {
		if strings.HasPrefix(text, prefix) {
			operator = prefix
			text = strings.TrimPrefix(text, prefix)
			break
		}
	}
	partial, err := parseNodePartial(text)
	if err != nil {
		return nil, err
	}
	if partial.parts == 0 {
		if operator == "<" || operator == ">" {
			return []nodeComparator{{operator: "never"}}, nil
		}
		return nil, nil
	}
	lower := partial.value
	if operator == "^" || operator == "~" || operator == "~>" {
		increment := 0
		if operator != "^" {
			if partial.parts >= 2 {
				increment = 1
			}
		} else if partial.parts >= 2 && lower[0] == 0 {
			increment = 1
			if partial.parts == 3 && lower[1] == 0 {
				increment = 2
			}
		}
		upper, err := nodeNext(lower, increment)
		if err != nil {
			return nil, err
		}
		return []nodeComparator{{">=", lower}, {"<", upper}}, nil
	}
	if partial.parts == 3 {
		if operator == "" {
			operator = "="
		}
		return []nodeComparator{{operator, lower}}, nil
	}
	// Missing/wildcard components are zeroed. > advances the unspecified
	// component's parent; <= includes that parent's entire version interval.
	switch operator {
	case ">=", "<":
		return []nodeComparator{{operator, lower}}, nil
	case ">", "<=", "", "=":
		upper, err := nodeNext(lower, partial.parts-1)
		if err != nil {
			return nil, err
		}
		switch operator {
		case ">":
			return []nodeComparator{{">=", upper}}, nil
		case "<=":
			return []nodeComparator{{"<", upper}}, nil
		default:
			return []nodeComparator{{">=", lower}, {"<", upper}}, nil
		}
	}
	return nil, errors.New("unsupported comparator")
}

func nodeNext(value nodeVersion, component int) (nodeVersion, error) {
	if value[component] == nodeMaxComponent {
		return nodeVersion{}, errors.New("expanded range exceeds npm's safe integer bound")
	}
	value[component]++
	for i := component + 1; i < len(value); i++ {
		value[i] = 0
	}
	return value, nil
}
