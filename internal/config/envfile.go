package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigPathVar names an explicit config file, overriding discovery.
const ConfigPathVar = "CN4M_CASCADE_CONFIG"

// envFileName is the file looked for beside the executable.
//
// The same name and format as the compose deployment's file, deliberately: an
// operator moving from a container to a native install should not have to learn
// a second configuration language, and the settings are identical.
const envFileName = ".env"

// LoadEnvFile reads KEY=value lines into a map.
//
// The format is the one `.env.example` already documents: `KEY=value`, `#`
// comments, blank lines ignored. Three tolerances exist because of how these
// files actually get written rather than how they ought to be:
//
//   - **A UTF-8 BOM is stripped.** Notepad writes one by default, and without
//     this the first key becomes U+FEFF followed by "ENCRYPTION_KEY" — a variable nobody
//     reads, producing "ENCRYPTION_KEY is not set" over a file that plainly
//     sets it.
//   - **CRLF is stripped.** Every Windows editor writes it, and a trailing
//     carriage return inside a value silently becomes part of the key, which
//     for an encryption key means nothing decrypts.
//   - **`export KEY=value` and surrounding quotes are accepted**, because
//     people paste from shell scripts and from other .env files.
//
// Values are otherwise taken literally: no interpolation, no escape sequences.
// A key that appears twice keeps the last occurrence, matching how a shell
// would treat successive assignments.
func LoadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	line := 0

	for scanner.Scan() {
		line++
		text := scanner.Text()
		if line == 1 {
			text = strings.TrimPrefix(text, "\ufeff")
		}
		text = strings.TrimSpace(strings.TrimSuffix(text, "\r"))

		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")

		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: %q is not KEY=value", path, line, text)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, line)
		}

		value = strings.TrimSpace(value)
		value = unquote(value)
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return values, nil
}

// unquote removes one layer of matching surrounding quotes.
//
// Only a matching pair, and only the outermost: a value that legitimately
// contains a quote — which a generated password can — must survive intact.
func unquote(v string) string {
	if len(v) < 2 {
		return v
	}
	first, last := v[0], v[len(v)-1]
	if first == last && (first == '"' || first == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// envFilePath returns the config file to read, and whether one was found.
//
// CN4M_CASCADE_CONFIG wins if set, and a missing file named that way is an
// error rather than a shrug — someone who pointed at a path meant it.
//
// Otherwise the file is looked for **beside the executable**, and nowhere else.
// Not the working directory: a Windows service runs with its working directory
// in system32, so a cwd-relative search would find nothing in the one
// deployment that most needs a config file, and would behave differently
// depending on where someone happened to be standing when they started it.
func envFilePath() (string, bool, error) {
	if explicit := strings.TrimSpace(os.Getenv(ConfigPathVar)); explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", false, fmt.Errorf("%s points at %s, which cannot be read: %w", ConfigPathVar, explicit, err)
		}
		return explicit, true, nil
	}

	// Both of the following are "no config file", not failures, so they are
	// asked as questions rather than as errors: having no .env beside the
	// executable is the ordinary case, and it is how every deployment worked
	// before this existed.
	dir, ok := executableDir()
	if !ok {
		return "", false, nil
	}
	candidate := filepath.Join(dir, envFileName)
	if !fileExists(candidate) {
		return "", false, nil
	}
	return candidate, true, nil
}

// executableDir returns the directory holding this binary.
func executableDir() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	return filepath.Dir(exe), true
}

// fileExists answers the only question the caller has. Returning a bool rather
// than an error is the point: "there is no config file" is not a problem to
// report, and an error here would have to be discarded at every call site.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// applyEnvFile loads the config file, if there is one, into the process
// environment without overwriting anything already set.
//
// **The real environment wins.** That is the same precedence `docker compose`
// gives its `.env`, and it is the order that makes a one-off override possible:
// `$env:LOG_LEVEL="debug"; .\cn4m-cascade.exe` has to work without editing a
// file. The reverse would make an exported variable silently ineffective, which
// is a bad afternoon for whoever is debugging it.
//
// Returns the path it used, or "" when there was no file.
func applyEnvFile() (string, error) {
	path, found, err := envFilePath()
	if err != nil || !found {
		return "", err
	}

	values, err := LoadEnvFile(path)
	if err != nil {
		return "", err
	}
	for key, value := range values {
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return "", fmt.Errorf("applying %s from %s: %w", key, path, err)
		}
	}
	return path, nil
}
