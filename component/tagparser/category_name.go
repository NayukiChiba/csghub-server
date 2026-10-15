package tagparser

import "strings"

const (
	categoryNameTask      = "task"
	categoryNameLicense   = "license"
	categoryNameFramework = "framework"
	categoryNameSize      = "size"
	categoryNameLanguage  = "language"
)

func formatCategoryName(name string) string {
	switch {
	//category task
	case strings.EqualFold(name, categoryNameTask):
		return categoryNameTask
	case strings.EqualFold(name, "tasks"):
		return categoryNameTask
	case strings.EqualFold(name, "tags"):
		return categoryNameTask
	case strings.EqualFold(name, "task_categories"):
		return categoryNameTask
	// HF cards also carry finer-grained task_ids (e.g. multiple-choice next to
	// the broader question-answering task_category); they are task tags too
	case strings.EqualFold(name, "task_ids"):
		return categoryNameTask
	case strings.EqualFold(name, "pipeline_tag"):
		return categoryNameTask

	//category license
	case strings.EqualFold(name, categoryNameLicense):
		return categoryNameLicense

	//category framework
	case strings.EqualFold(name, categoryNameFramework):
		return categoryNameFramework

	//category size
	case strings.EqualFold(name, categoryNameSize):
		return categoryNameSize
	case strings.EqualFold(name, "size_categories"):
		return categoryNameSize
	default:
		return name
	}
}
