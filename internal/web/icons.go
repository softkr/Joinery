package web

import "html/template"

// icon renders an <svg> that references a symbol from the "sprite" template.
// name is always a template literal, never user input.
func icon(name string) template.HTML {
	return template.HTML(`<svg class="i i-` + name + `" aria-hidden="true"><use href="#i-` + name + `"/></svg>`)
}
