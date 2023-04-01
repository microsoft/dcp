package output

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOmitDelimiterFormatting(t *testing.T) {
	a := []string{"alpha", "bravo", "charlie"}
	res := fmt.Sprintf("%#v", OmitSliceBrackets[string](a))
	require.Equal(t, "alpha bravo charlie", res)
}
