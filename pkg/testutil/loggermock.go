package testutil

import (
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/mock"
)

type MockLoggerSink struct {
	mock.Mock
}

func (m *MockLoggerSink) Enabled(level int) bool {
	args := m.Called(level)
	return args.Bool(0)
}

func (m *MockLoggerSink) Error(err error, msg string, keysAndValues ...interface{}) {
	m.Called(err, msg, keysAndValues)
}

func (m *MockLoggerSink) Info(level int, msg string, keysAndValues ...interface{}) {
	m.Called(level, msg, keysAndValues)
}

func (m *MockLoggerSink) Init(info logr.RuntimeInfo) {
	m.Called(info)
}

func (m *MockLoggerSink) WithName(name string) logr.LogSink {
	args := m.Called(name)
	return args.Get(0).(logr.LogSink)
}

func (m *MockLoggerSink) WithValues(keysAndValues ...interface{}) logr.LogSink {
	args := m.Called(keysAndValues)
	return args.Get(0).(logr.LogSink)
}

var _ logr.LogSink = (*MockLoggerSink)(nil)
