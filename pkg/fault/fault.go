// Package fault is the coded error shared by every package. Return a coded
// error only when the code is something other than Internal; plain errors
// are treated as Internal and never shown to a caller.
package fault

import (
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
)

type Code int

const (
	Internal Code = iota
	NotFound
	InvalidArgument
	PermissionDenied
	Unauthenticated
	Conflict
)

type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func New(code Code, message string) error {
	return &Error{Code: code, Message: message}
}

func Wrap(code Code, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func CodeOf(err error) Code {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return Internal
}

// Name is the code as a stable lower case word, for logs and the chain.
func Name(code Code) string {
	switch code {
	case NotFound:
		return "not_found"
	case InvalidArgument:
		return "invalid_argument"
	case PermissionDenied:
		return "permission_denied"
	case Unauthenticated:
		return "unauthenticated"
	case Conflict:
		return "conflict"
	}
	return "internal"
}

// Message is what a caller may see: the coded message, or a generic one.
func Message(err error) string {
	var coded *Error
	if errors.As(err, &coded) && coded.Code != Internal {
		return coded.Message
	}
	return "internal error"
}

// FromDb reads a refusal by the database as a coded error: a trigger or a
// policy that raised insufficient privilege is a PermissionDenied with the
// trigger's own message, a check or foreign key is an InvalidArgument, a
// unique violation a Conflict. Anything else stays as it is, so a real
// failure is still Internal and retried where that makes sense.
func FromDb(err error) error {
	if err == nil {
		return nil
	}
	var coded *Error
	if errors.As(err, &coded) {
		return err
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "42501":
		return Wrap(PermissionDenied, "refused by the database: "+pgErr.Message, err)
	// Check and foreign-key violations, and values the database cannot
	// take: bad text for a type, a NUL or unconvertible character, a
	// number out of range.
	case "23514", "23503", "22P02", "22P05", "22021", "22003":
		return Wrap(InvalidArgument, "refused by the database: "+pgErr.Message, err)
	case "23505":
		return Wrap(Conflict, "already exists", err)
	}
	return err
}

// ToConnect maps an error to a Connect error. Internal errors never expose
// their message; they are logged instead.
func ToConnect(err error) error {
	if err == nil {
		return nil
	}
	var coded *Error
	if !errors.As(err, &coded) || coded.Code == Internal {
		slog.Error("internal error", "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewError(connectCode(coded.Code), errors.New(coded.Message))
}

func connectCode(code Code) connect.Code {
	switch code {
	case NotFound:
		return connect.CodeNotFound
	case InvalidArgument:
		return connect.CodeInvalidArgument
	case PermissionDenied:
		return connect.CodePermissionDenied
	case Unauthenticated:
		return connect.CodeUnauthenticated
	case Conflict:
		return connect.CodeAlreadyExists
	default:
		return connect.CodeInternal
	}
}
