// Package parallax implements the transport-independent v1 handoff contract.
// Objects and json.Number values preserve additive fields and numeric precision.
package parallax

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Object = map[string]any

const MaxInputBytes = 2 << 20
const MaxAttempts = 3

var requirementPattern = regexp.MustCompile(`^([a-z][a-z0-9._-]*)(?:\s*(>=|<=|>|<|=|\^|~)\s*([0-9][a-zA-Z0-9.*+-]*))?$`)
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var numericVersionPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){0,3}$`)

// Decode accepts one bounded JSON object. Duplicate keys are rejected rather
// than allowing different consumers to bind different values to an approval.
func Decode(reader io.Reader) (Object, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxInputBytes {
		return nil, errors.New("JSON input exceeds 2 MiB")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("JSON input must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON input")
		}
		return nil, fmt.Errorf("trailing JSON input: %w", err)
	}
	object, ok := value.(Object)
	if !ok {
		return nil, errors.New("JSON input must be an object")
	}
	return object, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON nesting exceeds 128 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := Object{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key must be a string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return object, nil
	case '[':
		list := []any{}
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return list, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// Canonical sorts object keys and retains list order and number precision.
// json.Number lexical forms are retained; arbitrary PHP decimal/exponent
// normalization is not part of this dialect's hashing contract.
func Canonical(value any) ([]byte, error) {
	// Normalize structs, typed maps and custom JSON values to JSON objects before
	// serialization, so their object keys follow the same ordering as Object.
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	normalized, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func Hash(value any) (string, error) {
	data, err := Canonical(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func ParseRequirement(requirement string) (name, constraint string, err error) {
	parts := requirementPattern.FindStringSubmatch(requirement)
	if parts == nil {
		return "", "", fmt.Errorf("invalid capability requirement %q", requirement)
	}
	return parts[1], parts[2] + parts[3], nil
}

// StringList validates the unique, nonempty string lists used by v1 envelopes.
func StringList(value any, field string) ([]string, error) {
	var list []string
	switch values := value.(type) {
	case []string:
		if values == nil {
			return nil, fmt.Errorf("%s must be an array", field)
		}
		list = append([]string{}, values...)
	case []any:
		if values == nil {
			return nil, fmt.Errorf("%s must be an array", field)
		}
		list = make([]string, 0, len(values))
		for _, value := range values {
			item, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s must contain strings", field)
			}
			list = append(list, item)
		}
	default:
		return nil, fmt.Errorf("%s must be an array", field)
	}
	seen := map[string]bool{}
	for _, item := range list {
		if strings.TrimSpace(item) == "" || seen[item] {
			return nil, fmt.Errorf("%s must contain unique nonempty strings", field)
		}
		seen[item] = true
	}
	return list, nil
}

func requirements(value any, field string) ([]string, error) {
	list, err := StringList(value, field)
	if err != nil {
		return nil, err
	}
	for _, requirement := range list {
		if _, _, err := ParseRequirement(requirement); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func nonempty(object Object, field string) (string, error) {
	value, ok := object[field].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a nonempty string", field)
	}
	return value, nil
}

func ValidateRequest(request Object) error {
	if request["schema"] != "parallax.handoff/v1" {
		return errors.New("expected parallax.handoff/v1")
	}
	for _, field := range []string{"handoff_id", "mission_id", "step_id", "from", "to", "owner", "objective"} {
		if _, err := nonempty(request, field); err != nil {
			return err
		}
	}
	if request["owner"] != request["to"] {
		return errors.New("recipient must be the current owner")
	}
	if _, err := requirements(request["requires"], "requires"); err != nil {
		return err
	}
	if _, err := StringList(request["context_refs"], "context_refs"); err != nil {
		return err
	}
	attempt, err := integer(request["attempt"])
	if err != nil || attempt < 1 || attempt > MaxAttempts {
		return errors.New("attempt must be an integer between 1 and 3")
	}
	if context, exists := request["context"]; exists {
		if object, ok := context.(Object); !ok || object == nil {
			return errors.New("context must be an object")
		}
	}
	if parent, exists := request["parent_step_id"]; exists && parent != nil {
		if _, ok := parent.(string); !ok {
			return errors.New("parent_step_id must be a string or null")
		}
	}
	return nil
}

func integer(value any) (int, error) {
	switch value := value.(type) {
	case int:
		return value, nil
	case int64:
		if int64(int(value)) == value {
			return int(value), nil
		}
	case json.Number:
		parsed, err := strconv.ParseInt(string(value), 10, 32)
		return int(parsed), err
	}
	return 0, errors.New("not an integer")
}

// ValidateResult checks the result partition, fresh verification claims,
// ownership and bounded monotonic history. The caller persists that history.
func ValidateResult(result, request Object, history []Object) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	if err := validateResult(result); err != nil {
		return err
	}
	if result["handoff_id"] != request["handoff_id"] {
		return errors.New("result belongs to another handoff")
	}
	if owner, exists := result["owner"]; exists && owner != request["owner"] {
		return errors.New("only the current owner may complete a handoff")
	}
	satisfied, _ := requirements(result["satisfied"], "satisfied")
	remaining, _ := requirements(result["remaining"], "remaining")
	requires, _ := requirements(request["requires"], "requires")
	requested := set(requires)
	for _, item := range append(append([]string{}, satisfied...), remaining...) {
		if !requested[item] {
			return errors.New("result introduced work outside the handoff")
		}
		delete(requested, item)
	}
	if len(requested) != 0 {
		return errors.New("result omitted requested work")
	}
	attempt, _ := integer(request["attempt"])
	if len(remaining) != 0 && result["next_owner"] == request["owner"] && attempt >= MaxAttempts {
		return errors.New("attempt limit reached; remaining work must transfer")
	}
	sort.Strings(remaining)
	stalled := 0
	var preceding map[string]bool
	for _, previous := range history {
		if err := validateResult(previous); err != nil {
			return fmt.Errorf("invalid result history: %w", err)
		}
		prior, _ := requirements(previous["remaining"], "remaining")
		if preceding != nil {
			for _, item := range prior {
				if !preceding[item] {
					return errors.New("remaining work expanded within result history")
				}
			}
		}
		priorSet := set(prior)
		preceding = priorSet
		for _, item := range remaining {
			if !priorSet[item] {
				return errors.New("remaining work must shrink across a handoff chain")
			}
		}
		sort.Strings(prior)
		if equalStrings(prior, remaining) {
			stalled++
		} else {
			stalled = 0
		}
	}
	if len(remaining) != 0 && stalled >= MaxAttempts-1 {
		return errors.New("unchanged remaining work exceeded the loop limit")
	}
	return nil
}

func validateResult(result Object) error {
	if result["schema"] != "parallax.handoff-result/v1" {
		return errors.New("expected parallax.handoff-result/v1")
	}
	for _, field := range []string{"handoff_id", "status", "next_owner"} {
		if _, err := nonempty(result, field); err != nil {
			return err
		}
	}
	if owner, exists := result["owner"]; exists {
		if value, ok := owner.(string); !ok || strings.TrimSpace(value) == "" {
			return errors.New("owner must be a nonempty string")
		}
	}
	satisfied, err := requirements(result["satisfied"], "satisfied")
	if err != nil {
		return err
	}
	remaining, err := requirements(result["remaining"], "remaining")
	if err != nil {
		return err
	}
	remainingSet := set(remaining)
	for _, item := range satisfied {
		if remainingSet[item] {
			return errors.New("satisfied and remaining must be disjoint")
		}
	}
	status := "satisfied"
	if len(remaining) != 0 {
		status = "partial"
		if len(satisfied) == 0 {
			status = "unsatisfied"
		}
	}
	if result["status"] != "failed" && result["status"] != status {
		return errors.New("result status does not match its work partition")
	}
	if _, ok := array(result["artifacts"]); !ok {
		return errors.New("artifacts must be an array")
	}
	evidence, ok := array(result["evidence"])
	if !ok {
		return errors.New("evidence must be an array")
	}
	proofs := []Object{}
	for _, value := range evidence {
		proof, ok := value.(Object)
		if !ok || proof == nil {
			return errors.New("evidence entries must be objects")
		}
		for _, field := range []string{"type", "capability", "status"} {
			if _, err := nonempty(proof, field); err != nil {
				return err
			}
		}
		proofs = append(proofs, proof)
	}
	for _, requirement := range satisfied {
		name, constraint, _ := ParseRequirement(requirement)
		verified := false
		for _, proof := range proofs {
			if proof["status"] != "ok" || (proof["type"] != "verification" && proof["type"] != "already_installed") || (proof["capability"] != requirement && proof["capability"] != name) {
				continue
			}
			version, _ := proof["version"].(string)
			if constraint == "" || versionFits(version, constraint) {
				verified = true
				break
			}
		}
		if !verified {
			return fmt.Errorf("satisfied capability lacks compatible verification evidence: %s", requirement)
		}
	}
	return nil
}

func array(value any) ([]any, bool) {
	switch value := value.(type) {
	case []any:
		return value, value != nil
	case []Object:
		if value == nil {
			return nil, false
		}
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = item
		}
		return result, true
	case []string:
		if value == nil {
			return nil, false
		}
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = item
		}
		return result, true
	}
	return nil, false
}

func set(values []string) map[string]bool {
	result := map[string]bool{}
	for _, value := range values {
		result[value] = true
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// Only stable dotted numeric versions are supported. Prerelease, wildcard and
// arbitrary PHP version strings fail closed instead of claiming satisfaction.
func versionFits(version, constraint string) bool {
	parts := requirementPattern.FindStringSubmatch("capability" + constraint)
	if parts == nil {
		return false
	}
	actual, ok := numericVersion(version)
	if !ok {
		return false
	}
	minimum, ok := numericVersion(parts[3])
	if !ok {
		return false
	}
	comparison := compareVersion(actual, minimum)
	switch parts[2] {
	case "=":
		return comparison == 0
	case ">=":
		return comparison >= 0
	case "<=":
		return comparison <= 0
	case ">":
		return comparison > 0
	case "<":
		return comparison < 0
	case "^", "~":
		index := 0
		if parts[2] == "~" && len(minimum) >= 3 {
			index = 1
		}
		if parts[2] == "^" {
			for minimum[index] == 0 && index < len(minimum)-1 {
				index++
			}
		}
		length := len(minimum)
		if length < 3 {
			length = 3
		}
		upper := make([]uint64, length)
		copy(upper[:index], minimum[:index])
		if minimum[index] == ^uint64(0) {
			return false
		}
		upper[index] = minimum[index] + 1
		return comparison >= 0 && compareVersion(actual, upper) < 0
	}
	return false
}

func numericVersion(value string) ([]uint64, bool) {
	if !numericVersionPattern.MatchString(value) {
		return nil, false
	}
	segments := strings.Split(value, ".")
	result := make([]uint64, len(segments))
	for i, segment := range segments {
		parsed, err := strconv.ParseUint(segment, 10, 64)
		if err != nil {
			return nil, false
		}
		result[i] = parsed
	}
	return result, true
}

func compareVersion(left, right []uint64) int {
	length := len(left)
	if len(right) > length {
		length = len(right)
	}
	for i := 0; i < length; i++ {
		var l, r uint64
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if l < r {
			return -1
		}
		if l > r {
			return 1
		}
	}
	return 0
}

// ValidateApproval binds the complete body before selecting the executed subset
// and its transitive dependency closure. It does not authorize recipe execution.
func ValidateApproval(approval Object, selected []string, operation string) (Object, error) {
	if approval["schema"] != "archer.approval/v1" {
		return nil, errors.New("expected archer.approval/v1")
	}
	if operation != "install" && operation != "update" && operation != "remove" {
		return nil, errors.New("unsupported approval operation")
	}
	if approval["operation"] != operation {
		return nil, errors.New("operation differs from approved resolution")
	}
	digest, ok := approval["sha256"].(string)
	if !ok || !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid approval digest")
	}
	body := Object{}
	for key, value := range approval {
		if key != "sha256" {
			body[key] = value
		}
	}
	actual, err := Hash(body)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(actual), []byte(digest)) != 1 {
		return nil, errors.New("approved resolution digest mismatch")
	}
	approvedRequirements, err := requirements(approval["requirements"], "approval requirements")
	if err != nil {
		return nil, err
	}
	selected, err = requirements(selected, "selected requirements")
	if err != nil {
		return nil, err
	}
	allowed := set(approvedRequirements)
	for _, requirement := range selected {
		if !allowed[requirement] {
			return nil, errors.New("handoff requirements exceed approved resolution")
		}
	}
	nodes, ok := approval["nodes"].(Object)
	if !ok || nodes == nil {
		return nil, errors.New("approval nodes must be an object")
	}
	for name, value := range nodes {
		if !slugPattern.MatchString(name) {
			return nil, errors.New("approval node key must be a capability slug")
		}
		node, ok := value.(Object)
		if !ok || node == nil {
			return nil, fmt.Errorf("malformed approved dependency: %s", name)
		}
		if err := validateNode(name, node); err != nil {
			return nil, err
		}
	}
	closure := Object{}
	visiting := map[string]bool{}
	var visit func(string) error
	visit = func(requirement string) error {
		name, _, err := ParseRequirement(requirement)
		if err != nil {
			return err
		}
		if visiting[name] {
			return fmt.Errorf("approved dependency cycle at %s", name)
		}
		node, ok := nodes[name].(Object)
		if !ok {
			return fmt.Errorf("approved dependency binding missing: %s", name)
		}
		constraints, _ := requirements(node["version_constraint"], "version_constraint")
		if !set(constraints)[requirement] {
			return fmt.Errorf("approved constraint missing: %s", requirement)
		}
		if _, visited := closure[name]; visited {
			return nil
		}
		visiting[name] = true
		dependencies, _ := requirements(node["requires"], "requires")
		for _, dependency := range dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		delete(visiting, name)
		closure[name] = node
		return nil
	}
	for _, requirement := range selected {
		if err := visit(requirement); err != nil {
			return nil, err
		}
	}
	return closure, nil
}

func validateNode(name string, node Object) error {
	for _, field := range []string{"manifest", "manifest_sha256", "driver", "route_sha256"} {
		value, exists := node[field]
		if !exists {
			return fmt.Errorf("approved node %s lacks %s", name, field)
		}
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok || text == "" {
			return fmt.Errorf("approved node %s has invalid %s", name, field)
		}
		if strings.HasSuffix(field, "sha256") && !digestPattern.MatchString(text) {
			return fmt.Errorf("approved node %s has invalid %s", name, field)
		}
	}
	if _, err := requirements(node["requires"], "requires"); err != nil {
		return err
	}
	constraints, err := requirements(node["version_constraint"], "version_constraint")
	if err != nil {
		return err
	}
	if len(constraints) == 0 {
		return fmt.Errorf("approved node %s lacks version constraints", name)
	}
	for _, constraint := range constraints {
		capability, _, _ := ParseRequirement(constraint)
		if capability != name {
			return errors.New("approved constraint belongs to another capability")
		}
	}
	return nil
}

func NewApproval(selected []string, nodes Object, extensions Object) (Object, error) {
	selected, err := requirements(selected, "requirements")
	if err != nil {
		return nil, err
	}
	sort.Strings(selected)
	approval := Object{}
	for key, value := range extensions {
		if key == "schema" || key == "operation" || key == "requirements" || key == "nodes" || key == "sha256" {
			return nil, fmt.Errorf("approval extension cannot replace %s", key)
		}
		approval[key] = value
	}
	approval["schema"] = "archer.approval/v1"
	approval["operation"] = "install"
	approval["requirements"] = selected
	approval["nodes"] = nodes
	digest, err := Hash(approval)
	if err != nil {
		return nil, err
	}
	approval["sha256"] = digest
	if _, err := ValidateApproval(approval, selected, "install"); err != nil {
		return nil, err
	}
	return approval, nil
}
