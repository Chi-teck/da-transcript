package app

func titleOf(s *session) string {
	if s.title == "" {
		return "(untitled)"
	}
	return s.title
}
