package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxStartAttempts bounds how many times Up tries a gateway that lost its port.
const maxStartAttempts = 3

// errPortTaken says a gateway exited because another socket held the port it was
// given. Only this cause makes Up try again, on a new port.
var errPortTaken = errors.New("the gateway's port was taken before it could bind it")

// portTakenError is errPortTaken with the port and the end of the log. tries is set
// by Up when every attempt failed this way.
type portTakenError struct {
	port, tries int
	tail        string
}

func (e *portTakenError) Error() string {
	each := ""
	if e.tries > 1 {
		each = fmt.Sprintf(" on each of %d attempts", e.tries)
	}
	return fmt.Sprintf("sandbox up: the gateway could not bind its port%s (last: %d): address already in use%s", each, e.port, e.tail)
}

func (e *portTakenError) Is(target error) bool { return target == errPortTaken }

// logShowsPortTaken says whether the gateway log has the error the shared HTTP server
// dies with when it cannot bind port. It names the port, so a bind failure on another
// address is not taken for this one. The log is read here and never printed.
func logShowsPortTaken(path string, port int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > maxLogRead {
		_, _ = f.Seek(-maxLogRead, io.SeekEnd)
	}
	data, err := io.ReadAll(f)
	return err == nil && strings.Contains(string(data), fmt.Sprintf("listen tcp 127.0.0.1:%d: bind: address already in use", port))
}
