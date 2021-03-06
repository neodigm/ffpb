package main

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/cheggaaa/pb"
	"github.com/kr/pty"
	"github.com/mattn/go-isatty"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
)

var (
	ProgressTimeRegex = regexp.MustCompile(`\s+time=\s*((\d{2}):(\d{2}):(\d{2}))\.\d+`)
	DurationRegex     = regexp.MustCompile(`\s+Duration:\s*((\d{2}):(\d{2}):(\d{2}))\.\d+`)
	bar               *pb.ProgressBar
)

func splitLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte{'\r', '\n'}); i >= 0 {
		return i + 2, data[0 : i+2], nil
	}
	if i := bytes.Index(data, []byte{'[', 'y', '/', 'N', ']', ' '}); i == len(data)-6 {
		return i + 6, data[0 : i+6], nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if i == len(data)-1 && data[i] == '\r' {
			// ignore \r end of data
			return 0, nil, nil
		}
		return i + 1, data[0 : i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func findTime(re *regexp.Regexp, line string) (bool, int, string) {
	time := re.FindStringSubmatch(line)
	if len(time) == 5 {
		h, _ := strconv.Atoi(time[2])
		m, _ := strconv.Atoi(time[3])
		s, _ := strconv.Atoi(time[4])
		return true, h*3600 + m*60 + s, time[1]
	}
	return false, 0, ""
}

func renderProgress(duration int, line string, out *os.File) {
	if duration <= 0 {
		fmt.Fprint(out, line)
		return
	}
	exists, current, _ := findTime(ProgressTimeRegex, line)
	if !exists {
		fmt.Fprint(out, line)
		return
	}
	if isatty.IsTerminal(out.Fd()) {
		if bar == nil {
			bar = initProgressBar(duration, out)
		}
		bar.Prefix(line[:len(line)-1])
		bar.Set(current)
	} else {
		fmt.Fprint(out, line)
	}
}

func initProgressBar(duration int, out *os.File) *pb.ProgressBar {
	bar := pb.New(duration)
	bar.Output = out
	bar.SetUnits(pb.U_DURATION)
	bar.ShowCounters = false
	bar.ShowTimeLeft = false
	bar.Start()
	return bar
}

func readLine(in io.Reader, out *os.File) error {
	scanner := bufio.NewScanner(in)
	scanner.Split(splitLine)
	duration := 0
	for scanner.Scan() {
		lineBytes := scanner.Bytes()
		line := string(lineBytes)
		if lineBytes[len(lineBytes)-1] == '\r' {
			renderProgress(duration, line, out)
		} else {
			if bar != nil {
				bar.Set64(bar.Total)
				bar.Finish()
			}
			exists, t, _ := findTime(DurationRegex, line)
			if exists {
				duration = t
			}
			fmt.Fprint(out, line)
		}
	}
	return scanner.Err()
}

func catchTerminate(cmd *exec.Cmd) {
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGKILL)
	defer signal.Stop(signalCh)
	select {
	case ch := <-signalCh:
		if bar != nil {
			bar.Finish()
		}
		cmd.Process.Signal(ch)
		return
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `ffpb - Non-invasive progress bar for FFmpeg
usage: ffpb [command]
example:
	ffpb ffmpeg [options]
	ffmpeg [options] |& ffpb`)
	os.Exit(1)
}

func main() {

	if len(os.Args) == 1 {
		if isatty.IsTerminal(os.Stdin.Fd()) {
			usage()
		}
		readLine(os.Stdin, os.Stdout)
		return
	}

	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setctty: true,
		Setsid:  true,
	}

	stdinWriter, stdoutReader, stderrReader, closeAfterStart, closeAfterWait := redirect(cmd)

	err := cmd.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmd start error %s", err)
		os.Exit(1)
	}

	go io.Copy(stdinWriter, os.Stdin)
	go readLine(stdoutReader, os.Stdout)
	go readLine(stderrReader, os.Stderr)

	for _, c := range closeAfterStart {
		c.Close()
	}
	defer func() {
		for _, c := range closeAfterWait {
			c.Close()
		}
	}()

	go catchTerminate(cmd)

	cmd.Wait()
}

func redirect(cmd *exec.Cmd) (io.Writer, io.ReadCloser, io.ReadCloser, []io.Closer, []io.Closer) {
	var closeAfterStart []io.Closer
	var closeAfterWait []io.Closer

	ptyStdin, ttyStdin, err := pty.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pty open error %s", err)
		os.Exit(1)
	}
	cmd.Stdin = ttyStdin
	closeAfterStart = append(closeAfterStart, ttyStdin)
	closeAfterWait = append(closeAfterWait, ptyStdin)

	var stdoutReader io.ReadCloser
	if isatty.IsTerminal(os.Stdout.Fd()) {
		ptyStdout, ttyStdout, err := pty.Open()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pty open error %s", err)
			os.Exit(1)
		}
		cmd.Stdout = ttyStdout
		closeAfterStart = append(closeAfterStart, ttyStdout)
		closeAfterWait = append(closeAfterWait, ptyStdout)
		stdoutReader = ptyStdout
	} else {
		stdoutReader, err = cmd.StdoutPipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pipe error %s", err)
			os.Exit(1)
		}
	}

	var stderrReader io.ReadCloser
	if isatty.IsTerminal(os.Stderr.Fd()) {
		ptyStderr, ttyStderr, err := pty.Open()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pty open error %s", err)
			os.Exit(1)
		}
		stderrReader = ptyStderr
		closeAfterStart = append(closeAfterStart, ttyStderr)
		closeAfterWait = append(closeAfterWait, ptyStderr)
		cmd.Stderr = ttyStderr
	} else {
		stderrReader, err = cmd.StderrPipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pipe error %s", err)
			os.Exit(1)
		}
	}

	return ptyStdin, stdoutReader, stderrReader, closeAfterStart, closeAfterWait
}
