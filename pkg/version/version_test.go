package version

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFromBuild(t *testing.T) {
	for name, c := range map[string]struct {
		info debug.BuildInfo
		want string
	}{
		"a tag":            {debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, "v0.1.0"},
		"a pseudo-version": {debug.BuildInfo{Main: debug.Module{Version: "v0.0.2-0.20261125120000-0123456789ab+dirty"}}, "v0.0.2-0.20261125120000-0123456789ab+dirty"},
		"a commit": {debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"}, {Key: "vcs.modified", Value: "false"}}}, "dev-0123456789ab"},
		"a dirty commit": {debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"}, {Key: "vcs.modified", Value: "true"}}}, "dev-0123456789ab+dirty"},
		"nothing":           {debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
		"no module version": {debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}}, "dev-abc"},
	} {
		require.Equal(t, c.want, fromBuild(&c.info), name)
	}
}

func TestLinkTimeWins(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "v9.9.9"
	require.Equal(t, "v9.9.9", String())
}
