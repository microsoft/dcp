package process

type ProcessExitInfo struct {
	PID      int32
	ExitCode int32
	Err      error
}

func NewProcessExitInfo() ProcessExitInfo {
	return ProcessExitInfo{
		PID:      UnknownPID,
		ExitCode: UnknownExitCode,
		Err:      nil,
	}
}

// A simple process exit handler that writes the finished process status to a channel
type ChannelProcessExitHandler struct {
	c chan ProcessExitInfo
}

func NewChannelProcessExitHandler(c chan ProcessExitInfo) *ChannelProcessExitHandler {
	return &ChannelProcessExitHandler{
		c: c,
	}
}

func (eh *ChannelProcessExitHandler) OnProcessExited(pid int32, exitCode int32, err error) {
	eh.c <- ProcessExitInfo{
		PID:      pid,
		ExitCode: exitCode,
		Err:      err,
	}
	close(eh.c)
}
