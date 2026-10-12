package proc

import "os/exec"

// StartDetached starts argv without attaching its standard streams to the hook.
func StartDetached(argv, env []string, dir string) error {
	if len(argv) == 0 || argv[0] == "" {
		return exec.ErrNotFound
	}
	command := Hide(exec.Command(argv[0], argv[1:]...))
	command.Env = env
	command.Dir = dir
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	configureDetached(command)
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
