package buildinfo

import "fmt"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String(program string) string {
	return fmt.Sprintf(
		"%s %s (commit %s, built %s)",
		program,
		Version,
		Commit,
		Date,
	)
}

func Requested(arguments []string) bool {
	if len(arguments) != 1 {
		return false
	}
	switch arguments[0] {
	case "version", "--version":
		return true
	default:
		return false
	}
}
