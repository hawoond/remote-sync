package pathpolicy

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	MaxPathBytes    = 1024
	MaxSegmentBytes = 240
	MaxDepth        = 64
)

type Code string

const (
	CodeEmpty             Code = "EMPTY_PATH"
	CodeInvalidUTF8       Code = "INVALID_UTF8"
	CodeAbsolute          Code = "ABSOLUTE_PATH"
	CodeSeparator         Code = "INVALID_SEPARATOR"
	CodeSegment           Code = "INVALID_SEGMENT"
	CodeSegmentTooLong    Code = "SEGMENT_TOO_LONG"
	CodePathTooLong       Code = "PATH_TOO_LONG"
	CodeTooDeep           Code = "PATH_TOO_DEEP"
	CodeReservedCharacter Code = "RESERVED_CHARACTER"
	CodeReservedName      Code = "RESERVED_NAME"
	CodeTrailingDotSpace  Code = "TRAILING_DOT_OR_SPACE"
)

type Violation struct {
	Code Code
}

func (v *Violation) Error() string {
	return fmt.Sprintf("portable path policy violation: %s", v.Code)
}

type CanonicalPath struct {
	Display string
	Key     string
}

var (
	fold = cases.Fold()

	windowsReservedNames = map[string]struct{}{
		"CON":  {},
		"PRN":  {},
		"AUX":  {},
		"NUL":  {},
		"COM1": {},
		"COM2": {},
		"COM3": {},
		"COM4": {},
		"COM5": {},
		"COM6": {},
		"COM7": {},
		"COM8": {},
		"COM9": {},
		"LPT1": {},
		"LPT2": {},
		"LPT3": {},
		"LPT4": {},
		"LPT5": {},
		"LPT6": {},
		"LPT7": {},
		"LPT8": {},
		"LPT9": {},
	}
)

func Canonicalize(value string) (CanonicalPath, error) {
	if value == "" {
		return CanonicalPath{}, &Violation{Code: CodeEmpty}
	}
	if !utf8.ValidString(value) {
		return CanonicalPath{}, &Violation{Code: CodeInvalidUTF8}
	}
	if strings.ContainsRune(value, '\\') {
		return CanonicalPath{}, &Violation{Code: CodeSeparator}
	}
	if strings.HasPrefix(value, "/") || path.IsAbs(value) || hasDrivePrefix(value) {
		return CanonicalPath{}, &Violation{Code: CodeAbsolute}
	}

	normalized := norm.NFC.String(value)
	segments := strings.Split(normalized, "/")
	if len(segments) > MaxDepth {
		return CanonicalPath{}, &Violation{Code: CodeTooDeep}
	}

	for _, segment := range segments {
		if err := validateSegment(segment); err != nil {
			return CanonicalPath{}, err
		}
	}

	display := strings.Join(segments, "/")
	if len([]byte(display)) > MaxPathBytes {
		return CanonicalPath{}, &Violation{Code: CodePathTooLong}
	}

	return CanonicalPath{
		Display: display,
		Key:     norm.NFC.String(fold.String(display)),
	}, nil
}

func validateSegment(segment string) error {
	if segment == "" || segment == "." || segment == ".." {
		return &Violation{Code: CodeSegment}
	}
	if len([]byte(segment)) > MaxSegmentBytes {
		return &Violation{Code: CodeSegmentTooLong}
	}
	if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
		return &Violation{Code: CodeTrailingDotSpace}
	}

	for _, r := range segment {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
			return &Violation{Code: CodeReservedCharacter}
		}
	}

	base := segment
	if dot := strings.IndexRune(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	if _, reserved := windowsReservedNames[strings.ToUpper(base)]; reserved {
		return &Violation{Code: CodeReservedName}
	}
	return nil
}

func hasDrivePrefix(value string) bool {
	return len(value) >= 2 &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':'
}
