package core

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// courseTermPattern matches academic terms embedded in course names, such as
// "2025-2026-2高等数学".
var courseTermPattern = regexp.MustCompile(`(20[0-9]{2})-(20[0-9]{2})-([12])`)

// semesterBounds returns the start of the current semester and a comparable
// term key. September starts a new academic year; February through August
// still belong to the spring term.
func semesterBounds(now time.Time, timezone string) (time.Time, int) {
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "" {
		loc = time.UTC
	}
	local := now.In(loc)
	year, month := local.Year(), local.Month()
	switch {
	case month == time.January:
		return time.Date(year-1, time.September, 1, 0, 0, 0, 0, loc), (year - 1) * 2
	case month >= time.September:
		return time.Date(year, time.September, 1, 0, 0, 0, 0, loc), year * 2
	default:
		return time.Date(year, time.February, 15, 0, 0, 0, 0, loc), (year-1)*2 + 1
	}
}

func semesterCourseKey(t Task) string {
	course := t.Course
	if strings.TrimSpace(course) == "" {
		course = "#" + t.ID
	}
	return t.Platform + "\x00" + course
}

// markHistorical classifies tasks by course. A course belongs to an earlier
// semester when every dated task ends before this term, or, for courses
// without deadlines (videos), when the course name carries an older term.
// Courses without dates or term labels stay visible.
func markHistorical(tasks []Task, now time.Time, timezone string) {
	start, currentKey := semesterBounds(now, timezone)
	type group struct {
		dated, current, named bool
		namedKey              int
	}
	groups := make(map[string]*group, len(tasks))
	for i := range tasks {
		key := semesterCourseKey(tasks[i])
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		if end := tasks[i].EndAt; end != nil {
			g.dated = true
			if !end.Before(start) {
				g.current = true
			}
		}
		if !g.named {
			if match := courseTermPattern.FindStringSubmatch(tasks[i].Course); match != nil {
				year, yearErr := strconv.Atoi(match[1])
				term, termErr := strconv.Atoi(match[3])
				if yearErr == nil && termErr == nil {
					g.named = true
					g.namedKey = year*2 + term - 1
				}
			}
		}
	}
	for i := range tasks {
		g := groups[semesterCourseKey(tasks[i])]
		historical := false
		switch {
		case g.current:
		case g.dated:
			historical = true
		case g.named:
			historical = g.namedKey < currentKey
		}
		tasks[i].Historical = historical
	}
}
