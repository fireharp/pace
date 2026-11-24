package platform

// New returns the Backend for the current OS (set by platform_<os>.go init).
func New() (Backend, error) {
	if newNative == nil {
		return nil, errUnsupported
	}
	return newNative()
}

var newNative func() (Backend, error)

type unsupportedErr struct{}

func (unsupportedErr) Error() string { return "platform: unsupported operating system" }

var errUnsupported error = unsupportedErr{}

// QuickPressure returns the kernel memory-pressure level normalised to
// 0 unknown, 1 normal, 2 warn, 4 critical, without constructing a Backend.
// The wrapper uses it to refuse failing open into critical pressure.
// OS files set quickPressure in init().
func QuickPressure() int {
	if quickPressure == nil {
		return 0
	}
	return quickPressure()
}

var quickPressure func() int
