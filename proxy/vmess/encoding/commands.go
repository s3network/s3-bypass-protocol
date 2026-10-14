package encoding

import (
	"encoding/binary"
	"io"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/protocol"
)

var (
	ErrCommandTooLarge     = errors.New("Command too large.")
	ErrCommandTypeMismatch = errors.New("Command type mismatch.")
	ErrInvalidAuth         = errors.New("Invalid auth.")
	ErrInsufficientLength  = errors.New("Insufficient length.")
	ErrUnknownCommand      = errors.New("Unknown command.")
)

// MarshalCommand writes a response command frame.
//
// Command support is deliberately not implemented, so this always returns
// ErrUnknownCommand. Callers treat that as "this session carries no command"
// and carry on: server.go writes an empty command frame and client.go leaves
// header.Command unset. The body that used to live here was unreachable behind
// a switch with no arms; to add a command back, reintroduce a factory and
// dispatch on the command type rather than editing this function.
func MarshalCommand(command interface{}, writer io.Writer) error {
	if command == nil {
		return ErrUnknownCommand
	}
	return ErrUnknownCommand
}

// UnmarshalCommand reads a response command frame.
//
// As in MarshalCommand, no command types are registered, so this always
// returns ErrUnknownCommand. The framing is still validated first, so a peer
// sending a malformed frame gets ErrInsufficientLength or ErrInvalidAuth
// rather than being told the command type is merely unknown.
func UnmarshalCommand(cmdID byte, data []byte) (protocol.ResponseCommand, error) {
	if len(data) <= 4 {
		return nil, ErrInsufficientLength
	}
	expectedAuth := Authenticate(data[4:])
	actualAuth := binary.BigEndian.Uint32(data[:4])
	if expectedAuth != actualAuth {
		return nil, ErrInvalidAuth
	}
	return nil, ErrUnknownCommand
}

type CommandFactory interface {
	Marshal(command interface{}, writer io.Writer) error
	Unmarshal(data []byte) (interface{}, error)
}
