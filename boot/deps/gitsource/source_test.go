// SPDX-License-Identifier: MPL-2.0

package gitsource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSource(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"acme/http":                        false,
		"chicago/shell":                    false,
		"../shell":                         false,
		"./shell":                          false,
		"/home/me/shell":                   false,
		"":                                 false,
		"shell":                            false,
		"github.com/chicago-desktop/shell": true,
		"github.com/chicago-desktop/shell#v0.1.3":       true,
		"https://github.com/chicago-desktop/shell":      true,
		"https://github.com/chicago-desktop/shell.git":  true,
		"git+https://github.com/chicago-desktop/shell":  true,
		"ssh://git@github.com/chicago-desktop/shell":    true,
		"git@github.com:chicago-desktop/shell.git":      true,
		"git@github.com:chicago-desktop/shell.git#main": true,
		"/srv/git/shell.git":                            true,
		"/srv/git/shell.git#v1.0.0":                     true,
		"file:///srv/git/shell":                         true,
	}
	for value, want := range cases {
		assert.Equal(t, want, IsSource(value), value)
	}
}

func TestParse_EveryForm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value string
		url   string
		ref   string
		key   string
	}{
		{"github.com/chicago-desktop/shell", "https://github.com/chicago-desktop/shell", "", "github.com/chicago-desktop/shell"},
		{"github.com/chicago-desktop/shell#v0.1.3", "https://github.com/chicago-desktop/shell", "v0.1.3", "github.com/chicago-desktop/shell"},
		{"https://github.com/chicago-desktop/shell", "https://github.com/chicago-desktop/shell", "", "github.com/chicago-desktop/shell"},
		{"https://github.com/chicago-desktop/shell.git#main", "https://github.com/chicago-desktop/shell.git", "main", "github.com/chicago-desktop/shell"},
		{"git+https://github.com/chicago-desktop/shell.git", "https://github.com/chicago-desktop/shell.git", "", "github.com/chicago-desktop/shell"},
		{"ssh://git@github.com/chicago-desktop/shell.git", "ssh://git@github.com/chicago-desktop/shell.git", "", "github.com/chicago-desktop/shell"},
		{"git@github.com:chicago-desktop/shell.git", "git@github.com:chicago-desktop/shell.git", "", "github.com/chicago-desktop/shell"},
		{"git@github.com:chicago-desktop/shell.git#42c349180724271df4a875998b0c464942da881f", "git@github.com:chicago-desktop/shell.git", "42c349180724271df4a875998b0c464942da881f", "github.com/chicago-desktop/shell"},
		{"https://GitHub.com:443/Chicago-Desktop/Shell/", "https://GitHub.com:443/Chicago-Desktop/Shell/", "", "github.com/Chicago-Desktop/Shell"},
		{"/srv/git/shell.git#v1.0.0", "/srv/git/shell.git", "v1.0.0", "local/srv/git/shell"},
		{"file:///srv/git/shell.git", "file:///srv/git/shell.git", "", "local/srv/git/shell"},
	}
	for _, tc := range cases {
		src, err := Parse(tc.value)
		require.NoError(t, err, tc.value)
		assert.Equal(t, tc.value, src.Raw, tc.value)
		assert.Equal(t, tc.url, src.URL, tc.value)
		assert.Equal(t, tc.ref, src.Ref, tc.value)
		assert.Equal(t, tc.key, src.Key, tc.value)
	}
}

func TestParse_SameRepositoryAcrossSpellings(t *testing.T) {
	t.Parallel()
	a, err := Parse("github.com/chicago-desktop/shell")
	require.NoError(t, err)
	b, err := Parse("git@github.com:chicago-desktop/shell.git#main")
	require.NoError(t, err)
	c, err := Parse("https://github.com/chicago-desktop/weather")
	require.NoError(t, err)
	assert.True(t, a.SameRepository(b))
	assert.False(t, a.SameRepository(c))
}

func TestParse_Refuses(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "  ", "acme/http", "github.com/chicago-desktop/shell#", "https://", "github.com"} {
		_, err := Parse(value)
		assert.Error(t, err, value)
	}
}

func TestIsCommit(t *testing.T) {
	t.Parallel()
	assert.True(t, IsCommit("42c349180724271df4a875998b0c464942da881f"))
	assert.True(t, IsCommit("42C349180724271DF4A875998B0C464942DA881F"))
	assert.False(t, IsCommit("42c3491"))
	assert.False(t, IsCommit("main"))
}
