/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/mock"
)

type MockLoggerSink struct {
	mock.Mock
}

func NewMockLoggerSink() *MockLoggerSink {
	retval := &MockLoggerSink{}

	// Enable Init(), Enabled(), and Info() calls by default
	retval.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	retval.On("Enabled", mock.AnythingOfType("int")).Return(true)
	retval.On("Info", mock.AnythingOfType("int"), mock.AnythingOfType("string"), mock.Anything).Return()

	return retval
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

func (m *MockLoggerSink) EnableErrorCall() {
	m.On("Error", mock.AnythingOfType("*errors.errorString"), mock.AnythingOfType("string"), mock.Anything).Return()
}

var _ logr.LogSink = (*MockLoggerSink)(nil)
