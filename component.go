package mono

import "html/template"

type Component interface {
	Build(endpoints Endpoints) (template.HTML, error)
}
