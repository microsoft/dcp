package pointers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Checks if the EqualValue() function works correctly for K8s timestamps.
func TestEqualValueTimestamps(t *testing.T) {
	var t1, t2 *metav1.MicroTime

	require.Nil(t, t1)
	require.Nil(t, t2)
	require.True(t, EqualValue(t1, t2))

	now := metav1.NowMicro()
	now2 := metav1.NewMicroTime(now.Time)
	require.True(t, EqualValue(&now, &now2))

	require.False(t, EqualValue(&now, nil))
	require.False(t, EqualValue(nil, &now))

	later := metav1.NewMicroTime(now.Time.Add(1 * time.Second))
	require.False(t, EqualValue(&now, &later))
}

// Checks if the EqualValue() function works correctly for K8s durations.
func TestEqualValueDurations(t *testing.T) {
	var d1, d2 *metav1.Duration

	require.Nil(t, d1)
	require.Nil(t, d2)
	require.True(t, EqualValue(d1, d2))

	d1 = &metav1.Duration{Duration: 5 * time.Second}
	d2 = &metav1.Duration{Duration: 5 * time.Second}
	require.True(t, EqualValue(d1, d2))

	require.False(t, EqualValue(d1, nil))
	require.False(t, EqualValue(nil, d1))

	d3 := &metav1.Duration{Duration: 6 * time.Second}
	require.False(t, EqualValue(d1, d3))
}

// Check if SetValue() works correctly for K8s timestamps.
func TestSetValueTimestamps(t *testing.T) {
	require.Panics(t, func() { SetValue[metav1.MicroTime](nil, nil) })

	var t1, t2 *metav1.MicroTime
	require.Nil(t, t1)
	require.Nil(t, t2)

	SetValue(&t1, t2)
	require.Nil(t, t1)

	now := metav1.NowMicro()
	SetValue(&t1, &now)
	require.NotNil(t, t1)
	require.True(t, now.Equal(t1))
	require.Equal(t, now, *t1)

	later := metav1.NewMicroTime(now.Time.Add(1 * time.Second))
	SetValue(&t1, &later)
	require.NotNil(t, t1)
	require.True(t, later.Equal(t1))
	require.Equal(t, later, *t1)

	SetValue(&t1, nil)
	require.Nil(t, t1)
}

// Check if SetValue() works correctly for K8s durations.
func TestSetValueDurations(t *testing.T) {
	require.Panics(t, func() { SetValue[metav1.Duration](nil, nil) })

	var d1, d2 *metav1.Duration
	require.Nil(t, d1)
	require.Nil(t, d2)

	SetValue(&d1, d2)
	require.Nil(t, d1)

	d2 = &metav1.Duration{Duration: 5 * time.Second}
	SetValue(&d1, d2)
	require.NotNil(t, d1)
	require.Equal(t, 5*time.Second, d1.Duration)

	d3 := &metav1.Duration{Duration: 6 * time.Second}
	SetValue(&d1, d3)
	require.NotNil(t, d1)
	require.Equal(t, 6*time.Second, d1.Duration)

	SetValue(&d1, nil)
	require.Nil(t, d1)
}
