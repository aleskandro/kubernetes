/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apitesting "k8s.io/cri-api/pkg/apis/testing"
	"k8s.io/utils/pointer"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestLogOptions(t *testing.T) {
	var (
		line         = int64(8)
		bytes        = int64(64)
		timestamp    = metav1.Now()
		sinceseconds = int64(10)
	)
	for c, test := range []struct {
		apiOpts *v1.PodLogOptions
		expect  *LogOptions
	}{
		{ // empty options
			apiOpts: &v1.PodLogOptions{},
			expect:  &LogOptions{tail: -1, bytes: -1},
		},
		{ // test tail lines
			apiOpts: &v1.PodLogOptions{TailLines: &line},
			expect:  &LogOptions{tail: line, bytes: -1},
		},
		{ // test limit bytes
			apiOpts: &v1.PodLogOptions{LimitBytes: &bytes},
			expect:  &LogOptions{tail: -1, bytes: bytes},
		},
		{ // test since timestamp
			apiOpts: &v1.PodLogOptions{SinceTime: &timestamp},
			expect:  &LogOptions{tail: -1, bytes: -1, since: timestamp.Time},
		},
		{ // test since seconds
			apiOpts: &v1.PodLogOptions{SinceSeconds: &sinceseconds},
			expect:  &LogOptions{tail: -1, bytes: -1, since: timestamp.Add(-10 * time.Second)},
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		opts := NewLogOptions(test.apiOpts, timestamp.Time)
		assert.Equal(t, test.expect, opts)
	}
}

func TestReadLogs(t *testing.T) {
	file, err := os.CreateTemp("", "TestFollowLogs")
	if err != nil {
		t.Fatalf("unable to create temp file")
	}
	defer os.Remove(file.Name())
	file.WriteString(`{"log":"line1\n","stream":"stdout","time":"2020-09-27T11:18:01.00000000Z"}` + "\n")
	file.WriteString(`{"log":"line2\n","stream":"stdout","time":"2020-09-27T11:18:02.00000000Z"}` + "\n")
	file.WriteString(`{"log":"line3\n","stream":"stdout","time":"2020-09-27T11:18:03.00000000Z"}` + "\n")

	testCases := []struct {
		name          string
		podLogOptions v1.PodLogOptions
		expected      string
	}{
		{
			name:          "default pod log options should output all lines",
			podLogOptions: v1.PodLogOptions{},
			expected:      "line1\nline2\nline3\n",
		},
		{
			name: "using TailLines 2 should output last 2 lines",
			podLogOptions: v1.PodLogOptions{
				TailLines: pointer.Int64(2),
			},
			expected: "line2\nline3\n",
		},
		{
			name: "using TailLines 4 should output all lines when the log has less than 4 lines",
			podLogOptions: v1.PodLogOptions{
				TailLines: pointer.Int64(4),
			},
			expected: "line1\nline2\nline3\n",
		},
		{
			name: "using TailLines 0 should output nothing",
			podLogOptions: v1.PodLogOptions{
				TailLines: pointer.Int64(0),
			},
			expected: "",
		},
		{
			name: "using LimitBytes 9 should output first 9 bytes",
			podLogOptions: v1.PodLogOptions{
				LimitBytes: pointer.Int64(9),
			},
			expected: "line1\nlin",
		},
		{
			name: "using LimitBytes 100 should output all bytes when the log has less than 100 bytes",
			podLogOptions: v1.PodLogOptions{
				LimitBytes: pointer.Int64(100),
			},
			expected: "line1\nline2\nline3\n",
		},
		{
			name: "using LimitBytes 0 should output nothing",
			podLogOptions: v1.PodLogOptions{
				LimitBytes: pointer.Int64(0),
			},
			expected: "",
		},
		{
			name: "using SinceTime should output lines with a time on or after the specified time",
			podLogOptions: v1.PodLogOptions{
				SinceTime: &metav1.Time{Time: time.Date(2020, time.Month(9), 27, 11, 18, 02, 0, time.UTC)},
			},
			expected: "line2\nline3\n",
		},
		{
			name: "using SinceTime now should output nothing",
			podLogOptions: v1.PodLogOptions{
				SinceTime: &metav1.Time{Time: time.Now()},
			},
			expected: "",
		},
		{
			name: "using follow should output all log lines",
			podLogOptions: v1.PodLogOptions{
				Follow: true,
			},
			expected: "line1\nline2\nline3\n",
		},
		{
			name: "using follow combined with TailLines 2 should output the last 2 lines",
			podLogOptions: v1.PodLogOptions{
				Follow:    true,
				TailLines: pointer.Int64(2),
			},
			expected: "line2\nline3\n",
		},
		{
			name: "using follow combined with SinceTime should output lines with a time on or after the specified time",
			podLogOptions: v1.PodLogOptions{
				Follow:    true,
				SinceTime: &metav1.Time{Time: time.Date(2020, time.Month(9), 27, 11, 18, 02, 0, time.UTC)},
			},
			expected: "line2\nline3\n",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			containerID := "fake-container-id"
			fakeRuntimeService := &apitesting.FakeRuntimeService{
				Containers: map[string]*apitesting.FakeContainer{
					containerID: {
						ContainerStatus: runtimeapi.ContainerStatus{
							State: runtimeapi.ContainerState_CONTAINER_RUNNING,
						},
					},
				},
			}
			// If follow is specified, mark the container as exited or else ReadLogs will run indefinitely
			if tc.podLogOptions.Follow {
				fakeRuntimeService.Containers[containerID].State = runtimeapi.ContainerState_CONTAINER_EXITED
			}

			opts := NewLogOptions(&tc.podLogOptions, time.Now())
			stdoutBuf := bytes.NewBuffer(nil)
			stderrBuf := bytes.NewBuffer(nil)
			err = ReadLogs(context.TODO(), file.Name(), containerID, opts, fakeRuntimeService, stdoutBuf, stderrBuf)

			if err != nil {
				t.Fatalf(err.Error())
			}
			if stderrBuf.Len() > 0 {
				t.Fatalf("Stderr: %v", stderrBuf.String())
			}
			if actual := stdoutBuf.String(); tc.expected != actual {
				t.Fatalf("Actual output does not match expected.\nActual:  %v\nExpected: %v\n", actual, tc.expected)
			}
		})
	}
}

func TestParseLog(t *testing.T) {
	timestamp, err := time.Parse(timeFormatIn, "2016-10-20T18:39:20.57606443Z")
	assert.NoError(t, err)
	msg := &logMessage{}
	for c, test := range []struct {
		line string
		msg  *logMessage
		err  bool
	}{
		{ // Docker log format stdout
			line: `{"log":"docker stdout test log","stream":"stdout","time":"2016-10-20T18:39:20.57606443Z"}` + "\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("docker stdout test log"),
			},
		},
		{ // Docker log format stderr
			line: `{"log":"docker stderr test log","stream":"stderr","time":"2016-10-20T18:39:20.57606443Z"}` + "\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stderr,
				log:       []byte("docker stderr test log"),
			},
		},
		{ // CRI log format stdout
			line: "2016-10-20T18:39:20.57606443Z stdout F cri stdout test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("cri stdout test log\n"),
			},
		},
		{ // CRI log format stderr
			line: "2016-10-20T18:39:20.57606443Z stderr F cri stderr test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stderr,
				log:       []byte("cri stderr test log\n"),
			},
		},
		{ // Unsupported Log format
			line: "unsupported log format test log\n",
			msg:  &logMessage{},
			err:  true,
		},
		{ // Partial CRI log line
			line: "2016-10-20T18:39:20.57606443Z stdout P cri stdout partial test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("cri stdout partial test log"),
			},
		},
		{ // Partial CRI log line with multiple log tags.
			line: "2016-10-20T18:39:20.57606443Z stdout P:TAG1:TAG2 cri stdout partial test log\n",
			msg: &logMessage{
				timestamp: timestamp,
				stream:    runtimeapi.Stdout,
				log:       []byte("cri stdout partial test log"),
			},
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		parse, err := getParseFunc([]byte(test.line))
		if test.err {
			assert.Error(t, err)
			continue
		}
		assert.NoError(t, err)
		err = parse([]byte(test.line), msg)
		assert.NoError(t, err)
		assert.Equal(t, test.msg, msg)
	}
}

func TestWriteLogs(t *testing.T) {
	timestamp := time.Unix(1234, 43210)
	log := "abcdefg\n"

	for c, test := range []struct {
		stream       runtimeapi.LogStreamType
		since        time.Time
		timestamp    bool
		expectStdout string
		expectStderr string
	}{
		{ // stderr log
			stream:       runtimeapi.Stderr,
			expectStderr: log,
		},
		{ // stdout log
			stream:       runtimeapi.Stdout,
			expectStdout: log,
		},
		{ // since is after timestamp
			stream: runtimeapi.Stdout,
			since:  timestamp.Add(1 * time.Second),
		},
		{ // timestamp enabled
			stream:       runtimeapi.Stderr,
			timestamp:    true,
			expectStderr: timestamp.Format(timeFormatOut) + " " + log,
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		msg := &logMessage{
			timestamp: timestamp,
			stream:    test.stream,
			log:       []byte(log),
		}
		stdoutBuf := bytes.NewBuffer(nil)
		stderrBuf := bytes.NewBuffer(nil)
		w := newLogWriter(stdoutBuf, stderrBuf, &LogOptions{since: test.since, timestamp: test.timestamp, bytes: -1})
		err := w.write(msg, true)
		assert.NoError(t, err)
		assert.Equal(t, test.expectStdout, stdoutBuf.String())
		assert.Equal(t, test.expectStderr, stderrBuf.String())
	}
}

func TestWriteLogsWithBytesLimit(t *testing.T) {
	timestamp := time.Unix(1234, 4321)
	timestampStr := timestamp.Format(timeFormatOut)
	log := "abcdefg\n"

	for c, test := range []struct {
		stdoutLines  int
		stderrLines  int
		bytes        int
		timestamp    bool
		expectStdout string
		expectStderr string
	}{
		{ // limit bytes less than one line
			stdoutLines:  3,
			bytes:        3,
			expectStdout: "abc",
		},
		{ // limit bytes across lines
			stdoutLines:  3,
			bytes:        len(log) + 3,
			expectStdout: "abcdefg\nabc",
		},
		{ // limit bytes more than all lines
			stdoutLines:  3,
			bytes:        3 * len(log),
			expectStdout: "abcdefg\nabcdefg\nabcdefg\n",
		},
		{ // limit bytes for stderr
			stderrLines:  3,
			bytes:        len(log) + 3,
			expectStderr: "abcdefg\nabc",
		},
		{ // limit bytes for both stdout and stderr, stdout first.
			stdoutLines:  1,
			stderrLines:  2,
			bytes:        len(log) + 3,
			expectStdout: "abcdefg\n",
			expectStderr: "abc",
		},
		{ // limit bytes with timestamp
			stdoutLines:  3,
			timestamp:    true,
			bytes:        len(timestampStr) + 1 + len(log) + 2,
			expectStdout: timestampStr + " " + log + timestampStr[:2],
		},
	} {
		t.Logf("TestCase #%d: %+v", c, test)
		msg := &logMessage{
			timestamp: timestamp,
			log:       []byte(log),
		}
		stdoutBuf := bytes.NewBuffer(nil)
		stderrBuf := bytes.NewBuffer(nil)
		w := newLogWriter(stdoutBuf, stderrBuf, &LogOptions{timestamp: test.timestamp, bytes: int64(test.bytes)})
		for i := 0; i < test.stdoutLines; i++ {
			msg.stream = runtimeapi.Stdout
			if err := w.write(msg, true); err != nil {
				assert.EqualError(t, err, errMaximumWrite.Error())
			}
		}
		for i := 0; i < test.stderrLines; i++ {
			msg.stream = runtimeapi.Stderr
			if err := w.write(msg, true); err != nil {
				assert.EqualError(t, err, errMaximumWrite.Error())
			}
		}
		assert.Equal(t, test.expectStdout, stdoutBuf.String())
		assert.Equal(t, test.expectStderr, stderrBuf.String())
	}
}

func TestReadLogsLimitsWithTimestamps(t *testing.T) {
	logLineFmt := "2022-10-29T16:10:22.592603036-05:00 stdout P %v\n"
	logLineNewLine := "2022-10-29T16:10:22.592603036-05:00 stdout F \n"

	tmpfile, err := os.CreateTemp("", "log.*.txt")
	assert.NoError(t, err)

	count := 10000

	for i := 0; i < count; i++ {
		tmpfile.WriteString(fmt.Sprintf(logLineFmt, i))
	}
	tmpfile.WriteString(logLineNewLine)

	for i := 0; i < count; i++ {
		tmpfile.WriteString(fmt.Sprintf(logLineFmt, i))
	}
	tmpfile.WriteString(logLineNewLine)

	// two lines are in the buffer

	defer os.Remove(tmpfile.Name()) // clean up

	assert.NoError(t, err)
	tmpfile.Close()

	var buf bytes.Buffer
	w := io.MultiWriter(&buf)

	err = ReadLogs(context.Background(), tmpfile.Name(), "", &LogOptions{tail: -1, bytes: -1, timestamp: true}, nil, w, w)
	assert.NoError(t, err)

	lineCount := 0
	scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for scanner.Scan() {
		lineCount++

		// Split the line
		ts, logline, _ := bytes.Cut(scanner.Bytes(), []byte(" "))

		// Verification
		//   1. The timestamp should exist
		//   2. The last item in the log should be 9999
		_, err = time.Parse(time.RFC3339, string(ts))
		assert.NoError(t, err, "timestamp not found")
		assert.Equal(t, true, bytes.HasSuffix(logline, []byte("9999")), "is the complete log found")
	}

	assert.Equal(t, 2, lineCount, "should have two lines")
}
