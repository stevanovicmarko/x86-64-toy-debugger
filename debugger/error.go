package debugger

import "fmt"

// Error is a debugger-specific error type.
// It lets callers distinguish debugger errors from system errors.
type Error struct {
	msg string
}

func (e *Error) Error() string {
	return e.msg
}

// newError creates a debugger Error with the given message.
func newError(msg string) *Error {
	return &Error{msg: msg}
}

// newErrorf creates a debugger Error with a formatted message.
func newErrorf(format string, args ...any) *Error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}
