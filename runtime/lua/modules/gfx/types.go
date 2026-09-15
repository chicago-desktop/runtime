// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	typio "github.com/wippyai/go-lua/types/io"
	"github.com/wippyai/go-lua/types/typ"
)

// Координаты и размеры объявлены как `number`, а не `integer`, и это не
// небрежность: реализация НАМЕРЕННО принимает обе числовые формы Lua —
// литерал приезжает как LInteger, арифметика как LNumber, — и объявление,
// более строгое чем реализация, заставляет каждого вызывающего оборачивать
// `x + 1` в `math.tointeger`. Возвращаемые значения остаются целыми: там это
// правда.
//
// Найдено линтом, который до этого молчал: релизный рантайм типов `gfx` не
// знает вовсе и отвечал «чисто» на непроверенный код.
var fontType = typ.NewInterface("gfx.Font", []typ.Method{
	{Name: "size", Type: typ.Func().Param("self", typ.Self).Returns(typ.Number).Build()},
	{Name: "height", Type: typ.Func().Param("self", typ.Self).Returns(typ.Integer).Build()},
	{Name: "ascent", Type: typ.Func().Param("self", typ.Self).Returns(typ.Integer).Build()},
	{Name: "measure", Type: typ.Func().Param("self", typ.Self).Param("text", typ.String).
		Returns(typ.Integer, typ.Integer).Build()},
})

var rasterType = typ.NewInterface("gfx.Raster", []typ.Method{
	{Name: "size", Type: typ.Func().Param("self", typ.Self).Returns(typ.Integer, typ.Integer).Build()},
	{Name: "version", Type: typ.Func().Param("self", typ.Self).Returns(typ.Integer).Build()},
	{Name: "fill", Type: typ.Func().Param("self", typ.Self).Param("color", typ.String).Build()},
	{Name: "rect", Type: typ.Func().Param("self", typ.Self).
		Param("x", typ.Number).Param("y", typ.Number).
		Param("width", typ.Number).Param("height", typ.Number).
		Param("color", typ.String).Build()},
	{Name: "set", Type: typ.Func().Param("self", typ.Self).
		Param("x", typ.Number).Param("y", typ.Number).Param("color", typ.String).Build()},
	{Name: "text", Type: typ.Func().Param("self", typ.Self).
		Param("x", typ.Number).Param("y", typ.Number).
		Param("text", typ.String).
		Param("options", typ.NewRecord().
			Field("font", fontType).
			OptField("color", typ.String).
			OptField("smooth", typ.Boolean).
			Build()).
		Returns(typ.Integer).Build()},
	{Name: "blit", Type: typ.Func().Param("self", typ.Self).
		Param("source", typ.Self).
		Param("x", typ.Number).Param("y", typ.Number).
		OptParam("options", typ.NewRecord().
			OptField("rotate", typ.Number).
			Build()).Build()},
	{Name: "encode", Type: typ.Func().Param("self", typ.Self).
		OptParam("format", typ.String).
		Returns(typ.NewOptional(typ.String), typ.NewOptional(typ.String)).Build()},
	{Name: "scaled", Type: typ.Func().Param("self", typ.Self).
		Param("width", typ.Number).Param("height", typ.Number).
		OptParam("options", typ.NewRecord().
			OptField("smooth", typ.Boolean).
			Build()).
		Returns(typ.Self).Build()},
})

// ModuleTypes returns the type manifest for the gfx module.
func ModuleTypes() *typio.Manifest {
	m := typio.NewManifest("gfx")
	m.DefineType("Raster", rasterType)
	m.DefineType("Font", fontType)

	m.SetExport(typ.NewInterface("gfx", []typ.Method{
		{Name: "supported", Type: typ.Func().
			Returns(typ.NewOptional(typ.String), typ.NewOptional(typ.String)).Build()},
		{Name: "cell_size", Type: typ.Func().
			Returns(typ.NewOptional(typ.Integer), typ.NewOptional(typ.Any)).Build()},
		{Name: "raster", Type: typ.Func().
			Param("width", typ.Number).Param("height", typ.Number).
			Returns(rasterType).Build()},
		{Name: "image", Type: typ.Func().
			Param("data", typ.String).
			Returns(typ.NewOptional(rasterType), typ.NewOptional(typ.String)).Build()},
		{Name: "font", Type: typ.Func().
			Param("data", typ.String).
			OptParam("options", typ.NewRecord().
				OptField("size", typ.Number).
				OptField("smooth", typ.Boolean).
				Build()).
			Returns(fontType).Build()},
	}))
	return m
}
