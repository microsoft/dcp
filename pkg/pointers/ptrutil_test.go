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
	t1 = nil
	t2 = nil

	require.True(t, EqualValue(t1, t2))

	now := metav1.Now()
	now2 := metav1.NewTime(now.Time)
	require.True(t, EqualValue(&now, &now2))

	require.False(t, EqualValue(&now, nil))
	require.False(t, EqualValue(nil, &now))

	now3 := metav1.NewTime(now.Time.Add(1 * time.Second))
	require.False(t, EqualValue(&now, &now3))
}

// Checks if the EqualValue() function works correctly for K8s durations.
func TestEqualValueDurations(t *testing.T) {
	var d1, d2 *metav1.Duration
	d1 = nil
	d2 = nil

	require.True(t, EqualValue(d1, d2))

	d1 = &metav1.Duration{Duration: 5 * time.Second}
	d2 = &metav1.Duration{Duration: 5 * time.Second}
	require.True(t, EqualValue(d1, d2))

	require.False(t, EqualValue(d1, nil))
	require.False(t, EqualValue(nil, d1))

	d3 := &metav1.Duration{Duration: 6 * time.Second}
	require.False(t, EqualValue(d1, d3))
}
