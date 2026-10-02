package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// configFileCandidates are probed when config_path is not set explicitly.
var configFileCandidates = []string{
	"config.yaml",
	"../config.yaml",
	"../../config.yaml",
	"/CLIProxyAPI/config.yaml",
}

// resolveConfigFile locates config.yaml. The plugin runs in-process with the
// host, so an explicit config_path wins and the candidates are a fallback.
func resolveConfigFile() string {
	if configured := strings.TrimSpace(currentConfig().ConfigPath); configured != "" {
		if _, errStat := os.Stat(configured); errStat == nil {
			return configured
		}
	}
	if env := strings.TrimSpace(os.Getenv("CLIPROXY_CONFIG")); env != "" {
		if _, errStat := os.Stat(env); errStat == nil {
			return env
		}
	}
	for _, candidate := range configFileCandidates {
		if _, errStat := os.Stat(candidate); errStat == nil {
			absolute, errAbs := filepath.Abs(candidate)
			if errAbs == nil {
				return absolute
			}
			return candidate
		}
	}
	return ""
}

// applyConfigValues merges submitted dotted-key values into the plugin config,
// rewrites only the codex-policy block of config.yaml, and applies the result
// in-process so plugin-scoped settings take effect without a restart.
func applyConfigValues(raw []byte) (map[string]any, error) {
	var submitted map[string]any
	if errUnmarshal := json.Unmarshal(raw, &submitted); errUnmarshal != nil {
		return nil, fmt.Errorf("invalid json: %w", errUnmarshal)
	}
	if len(submitted) == 0 {
		return nil, fmt.Errorf("no values submitted")
	}

	path := resolveConfigFile()
	if path == "" {
		return nil, fmt.Errorf("config.yaml not found; set plugins.configs.codex-policy.config_path")
	}
	onDisk, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read config: %w", errRead)
	}

	merged := configuredRaw()
	for key, value := range submitted {
		doc, known := docFor(key)
		if !known {
			return nil, fmt.Errorf("unknown config key: %s", key)
		}
		coerced, errCoerce := coerceValue(doc, value)
		if errCoerce != nil {
			return nil, fmt.Errorf("%s: %w", key, errCoerce)
		}
		if errSet := setDotted(merged, key, coerced); errSet != nil {
			return nil, fmt.Errorf("%s: %w", key, errSet)
		}
	}

	block, errRender := renderPluginBlock(merged)
	if errRender != nil {
		return nil, errRender
	}
	updated, errSplice := splicePluginBlock(onDisk, block)
	if errSplice != nil {
		return nil, errSplice
	}
	if errValidate := validateConfigDocument(updated); errValidate != nil {
		return nil, errValidate
	}
	if errWrite := writeConfigAtomically(path, updated); errWrite != nil {
		return nil, errWrite
	}

	// Re-apply through the normal reconfigure path so defaults and clamping
	// stay identical to a host-driven reload.
	if errApply := applyConfigDocument(updated); errApply != nil {
		return nil, errApply
	}
	logf("config updated via management api: keys=%v file=%s", sortedKeys(submitted), path)
	return effectiveConfigPayload(), nil
}

func docFor(key string) (configFieldDoc, bool) {
	for _, doc := range configFieldDocs {
		if doc.Key == key {
			return doc, true
		}
	}
	return configFieldDoc{}, false
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

// coerceValue converts a form value (always a string in the browser) into the
// type declared for that config key.
func coerceValue(doc configFieldDoc, value any) (any, error) {
	if typed, ok := value.(bool); ok && doc.Type == "boolean" {
		return typed, nil
	}
	switch doc.Type {
	case "boolean":
		if typed, ok := value.(bool); ok {
			return typed, nil
		}
		text := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
		switch text {
		case "true", "1", "yes", "on", "是":
			return true, nil
		case "false", "0", "no", "off", "否":
			return false, nil
		}
		return nil, fmt.Errorf("expected a boolean, got %q", text)
	case "integer":
		switch typed := value.(type) {
		case float64:
			return int(typed), nil
		case int:
			return typed, nil
		}
		parsed, errParse := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value)))
		if errParse != nil {
			return nil, fmt.Errorf("expected an integer, got %q", value)
		}
		return parsed, nil
	case "array":
		switch typed := value.(type) {
		case []any:
			return typed, nil
		case []string:
			out := make([]any, 0, len(typed))
			for _, item := range typed {
				out = append(out, item)
			}
			return out, nil
		}
		text := strings.TrimSpace(fmt.Sprint(value))
		if text == "" {
			return []any{}, nil
		}
		if strings.HasPrefix(text, "[") {
			var parsed []any
			if errParse := json.Unmarshal([]byte(text), &parsed); errParse == nil {
				return parsed, nil
			}
		}
		var parsed []any
		if errParse := yaml.Unmarshal([]byte(text), &parsed); errParse == nil && parsed != nil {
			return parsed, nil
		}
		// Only fall back to comma splitting for a plain single-line list.
		// Text that actually looks like YAML (multi-line, or a leading "- ")
		// must parse as YAML: otherwise a typo in the rules textarea silently
		// becomes a list of one-off strings and the rules stop matching without
		// any error. A hyphen anywhere else is fine, so "sk-a, sk-b" still works.
		if strings.Contains(text, "\n") || strings.HasPrefix(text, "-") {
			return nil, fmt.Errorf("expected a yaml list, got %q", text)
		}
		parts := strings.Split(text, ",")
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
		return out, nil
	case "enum":
		text := strings.TrimSpace(fmt.Sprint(value))
		if len(doc.Enum) > 0 && !containsString(doc.Enum, text) {
			return nil, fmt.Errorf("expected one of %v, got %q", doc.Enum, text)
		}
		return text, nil
	default:
		if typed, ok := value.(string); ok {
			return typed, nil
		}
		return value, nil
	}
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func sortStrings(values []string) {
	sort.Strings(values)
}

func setDotted(root map[string]any, path string, value any) error {
	parts := strings.Split(path, ".")
	cursor := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := cursor[part]
		if !ok {
			return fmt.Errorf("unknown section %q", part)
		}
		asMap, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("section %q is not a mapping", part)
		}
		cursor = asMap
	}
	cursor[parts[len(parts)-1]] = value
	return nil
}

// renderPluginBlock renders the plugin config as a YAML block, annotating each
// documented key with its description so config.yaml stays self-documenting.
func renderPluginBlock(values map[string]any) (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(4)
	if errEncode := encoder.Encode(values); errEncode != nil {
		return "", fmt.Errorf("render config: %w", errEncode)
	}
	if errClose := encoder.Close(); errClose != nil {
		return "", fmt.Errorf("render config: %w", errClose)
	}
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Only top-level keys get a hint comment; a nested "enabled:" would
		// otherwise pick up the unrelated top-level enabled description.
		key, _, _ := strings.Cut(line, ":")
		if indentOf(line) == 0 {
			if doc, ok := docFor(key); ok {
				for _, comment := range commentLines(doc) {
					out.WriteString(comment)
					out.WriteString("\n")
				}
			}
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String(), nil
}

func commentLines(doc configFieldDoc) []string {
	parts := strings.Split(strings.ReplaceAll(doc.Description, "\r", ""), "\n")
	lines := make([]string, 0, len(parts)+1)
	lines = append(lines, fmt.Sprintf("# %s（默认 %s）", parts[0], doc.Default))
	for _, extra := range parts[1:] {
		if strings.TrimSpace(extra) == "" {
			continue
		}
		lines = append(lines, "# "+extra)
	}
	return lines
}

// splicePluginBlock replaces only the codex-policy block, leaving the rest of
// the document byte-identical so comments and formatting elsewhere survive.
func splicePluginBlock(document []byte, block string) ([]byte, error) {
	lines := strings.Split(string(document), "\n")
	start, indent, errFind := findPluginBlock(lines)
	if errFind != nil {
		return nil, errFind
	}
	end := pluginBlockEnd(lines, start, indent)
	parentPad := strings.Repeat(" ", indent)
	// Children of the plugin key sit one level deeper than the key itself.
	pad := strings.Repeat(" ", indent+4)
	var out strings.Builder
	for _, line := range lines[:start] {
		out.WriteString(line)
		out.WriteString("\n")
	}
	// The key line itself is part of the replaced range, so re-emit it.
	out.WriteString(parentPad)
	out.WriteString(pluginID)
	out.WriteString(":\n")
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		out.WriteString(pad)
		out.WriteString(line)
		out.WriteString("\n")
	}
	if end < len(lines) {
		out.WriteString(strings.Join(lines[end:], "\n"))
	}
	return []byte(out.String()), nil
}

// pluginBlockEnd returns the index just past the plugin block that starts at
// start. The block ends at the next real key at or below the plugin key's
// indent; blank lines and comments in between are part of the block.
//
// Stopping at the first blank line instead would be wrong: any key written
// after a blank line would survive the splice and then be duplicated under the
// rewritten block, producing duplicate keys and an unparseable config.yaml.
// Trailing blank lines and comments are handed back so separators between the
// plugin block and the next key survive untouched.
func pluginBlockEnd(lines []string, start, indent int) int {
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indentOf(lines[i]) <= indent {
			end = i
			break
		}
	}
	for end > start+1 {
		trimmed := strings.TrimSpace(lines[end-1])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		end--
	}
	return end
}

func findPluginBlock(lines []string) (int, int, error) {
	configsIndex, configsIndent := -1, 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "configs:" {
			configsIndex, configsIndent = i, indentOf(line)
			break
		}
	}
	if configsIndex < 0 {
		return 0, 0, fmt.Errorf("plugins.configs section not found in config.yaml")
	}
	for i := configsIndex + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indentOf(line) <= configsIndent {
			break
		}
		if strings.TrimSuffix(trimmed, ":") == pluginID {
			return i, indentOf(line), nil
		}
	}
	return 0, 0, fmt.Errorf("plugins.configs.%s not found in config.yaml", pluginID)
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func validateConfigDocument(document []byte) error {
	var probe map[string]any
	if errUnmarshal := yaml.Unmarshal(document, &probe); errUnmarshal != nil {
		return fmt.Errorf("rendered config is not valid yaml: %w", errUnmarshal)
	}
	return nil
}

// applyConfigDocument re-runs configure() with the freshly written document so
// the in-process state matches what the host would load after a restart.
func applyConfigDocument(document []byte) error {
	var root struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if errUnmarshal := yaml.Unmarshal(document, &root); errUnmarshal != nil {
		return errUnmarshal
	}
	node, ok := root.Plugins.Configs[pluginID]
	if !ok {
		return fmt.Errorf("plugins.configs.%s missing after write", pluginID)
	}
	block, errEncode := yaml.Marshal(&node)
	if errEncode != nil {
		return errEncode
	}
	return configureRaw(block)
}

// writeConfigAtomically writes a .bak copy first, then replaces config.yaml via
// a temp file in the same directory so a crash cannot truncate the original.
//
// config.yaml is normally bind-mounted as a single file (the common Docker
// deployment), and the kernel refuses to rename over a mount point with
// EBUSY. In that case we fall back to rewriting the file in place. The backup
// is already on disk by then and the document was validated before this call,
// so the fallback can only lose data on a hard crash mid-write, which the
// .bak file is there to recover from.
func writeConfigAtomically(path string, document []byte) error {
	if original, errRead := os.ReadFile(path); errRead == nil {
		_ = os.WriteFile(path+".bak-before-"+pluginID, original, 0o644)
	}
	dir := filepath.Dir(path)
	temp, errCreate := os.CreateTemp(dir, ".config.yaml.codex-policy-*")
	if errCreate != nil {
		return fmt.Errorf("create temp config: %w", errCreate)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if _, errWrite := temp.Write(document); errWrite != nil {
		_ = temp.Close()
		return fmt.Errorf("write temp config: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temp config: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temp config: %w", errClose)
	}
	if errInfo, errStat := os.Stat(path); errStat == nil {
		_ = os.Chmod(tempName, errInfo.Mode())
	}
	if errRename := os.Rename(tempName, path); errRename != nil {
		written, errWrite := os.ReadFile(tempName)
		if errWrite != nil {
			return fmt.Errorf("replace config: %w (and re-reading temp failed: %v)", errRename, errWrite)
		}
		if errTruncate := os.WriteFile(path, written, 0o644); errTruncate != nil {
			return fmt.Errorf("replace config: rename failed (%v) and in-place rewrite failed: %w", errRename, errTruncate)
		}
		logf("config.yaml is a bind mount; rename failed (%v), rewrote in place", errRename)
		return nil
	}
	return nil
}
