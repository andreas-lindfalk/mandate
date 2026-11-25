package mandates

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidKey(t *testing.T) {
	for key, want := range map[string]bool{
		"tickets.create": true, "a": true, "tickets.sub_area.close2": true,
		"": false, "Tickets.create": false, "tickets..create": false, ".tickets": false, "tickets.": false,
		"tickets.*": false, "*": false, "1tickets": false, "tickets-create": false,
	} {
		require.Equal(t, want, ValidKey(key), key)
	}
}

func TestValidPatternsAndCovers(t *testing.T) {
	require.True(t, ValidPatterns([]string{"*"}))
	require.True(t, ValidPatterns([]string{"tickets.*", "refunds.issue"}))
	require.False(t, ValidPatterns(nil))
	require.False(t, ValidPatterns([]string{"tickets.**"}))
	require.False(t, ValidPatterns([]string{"*.create"}))
	require.False(t, ValidPatterns([]string{""}))

	require.True(t, Covers([]string{"*"}, "anything.at.all"))
	require.True(t, Covers([]string{"tickets.*"}, "tickets.create"))
	require.True(t, Covers([]string{"tickets.*"}, "tickets.sub.close"))
	require.False(t, Covers([]string{"tickets.*"}, "ticketsx.create"), "a prefix matches whole segments only")
	require.False(t, Covers([]string{"tickets.*"}, "tickets"), "a prefix needs a segment after it")
	require.True(t, Covers([]string{"refunds.issue"}, "refunds.issue"))
	require.False(t, Covers([]string{"refunds.issue"}, "refunds.issue_large"))
}

func TestMandateAllows(t *testing.T) {
	m := Mandate{MaxRisk: RiskMedium}
	require.True(t, m.Allows(RiskLow))
	require.True(t, m.Allows(RiskMedium))
	require.False(t, m.Allows(RiskHigh))
	require.False(t, m.Allows("unknown"))
	require.False(t, Mandate{MaxRisk: RiskLow}.Allows(RiskMedium))
}

func TestPeriodIsTheUtcCalendarMonth(t *testing.T) {
	at := time.Date(2026, 11, 30, 23, 30, 0, 0, time.FixedZone("east", 2*3600))
	start, end := Period(at)
	require.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), start)
	require.Equal(t, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), end)

	start, _ = Period(time.Date(2026, 12, 1, 0, 30, 0, 0, time.FixedZone("east", 2*3600)))
	require.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), start, "late evening in the east is still November in UTC")
}
