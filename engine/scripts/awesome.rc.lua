pcall(require, "luarocks.loader")

local gears = require("gears")
local awful = require("awful")
require("awful.autofocus")
local wibox = require("wibox")
local beautiful = require("beautiful")

beautiful.init({
    font = "Sans 10",
    bg_normal = "#1f2329",
    bg_focus = "#2d333b",
    fg_normal = "#cdd9e5",
    fg_focus = "#ffffff",
    border_width = 1,
    border_normal = "#555555",
    border_focus = "#8b949e",
    titlebar_bg_normal = "#22272e",
    titlebar_bg_focus = "#2d333b",
    titlebar_fg_normal = "#cdd9e5",
    titlebar_fg_focus = "#ffffff",
})

awful.layout.layouts = {
    awful.layout.suit.tile,
    awful.layout.suit.max,
}

awful.screen.connect_for_each_screen(function(s)
    awful.tag({ "main" }, s, awful.layout.layouts[1])
end)

root.buttons(gears.table.join())

local titlebar_buttons = gears.table.join(
    awful.button({}, 1, function(c)
        c:activate { context = "titlebar", action = "mouse_move" }
    end),
    awful.button({}, 3, function(c)
        c:activate { context = "titlebar", action = "mouse_resize" }
    end)
)

awful.rules.rules = {
    {
        rule = {},
        properties = {
            border_width = beautiful.border_width,
            border_color = beautiful.border_normal,
            focus = awful.client.focus.filter,
            raise = true,
            screen = awful.screen.preferred,
            floating = false,
            fullscreen = false,
            maximized = false,
            above = false,
            ontop = false,
            sticky = false,
            titlebars_enabled = true,
            size_hints_honor = false,
        },
    },
}

client.connect_signal("request::titlebars", function(c)
    awful.titlebar(c, { size = 26 }):setup {
        {
            awful.titlebar.widget.iconwidget(c),
            buttons = titlebar_buttons,
            layout = wibox.layout.fixed.horizontal,
        },
        {
            align = "center",
            widget = awful.titlebar.widget.titlewidget(c),
            buttons = titlebar_buttons,
        },
        {
            awful.titlebar.widget.maximizedbutton(c),
            awful.titlebar.widget.closebutton(c),
            layout = wibox.layout.fixed.horizontal,
        },
        layout = wibox.layout.align.horizontal,
    }
end)

client.connect_signal("manage", function(c)
    c.floating = false
    c.fullscreen = false
    c.maximized = false
    c.maximized_horizontal = false
    c.maximized_vertical = false
    c.above = false
    c.ontop = false
    c.sticky = false
    c.size_hints_honor = false
    c:tags({ c.screen.selected_tag })
    if not awesome.startup then
        awful.client.setslave(c)
    end
end)

client.connect_signal("property::floating", function(c)
    if c.floating then
        c.floating = false
    end
end)

client.connect_signal("property::fullscreen", function(c)
    if c.fullscreen then
        c.fullscreen = false
    end
end)

client.connect_signal("property::maximized", function(c)
    if c.maximized then
        c.maximized = false
    end
end)

client.connect_signal("property::above", function(c)
    if c.above then
        c.above = false
    end
end)

client.connect_signal("property::ontop", function(c)
    if c.ontop then
        c.ontop = false
    end
end)

client.connect_signal("mouse::enter", function(c)
    c:activate { context = "mouse_enter", raise = false }
end)

client.connect_signal("focus", function(c)
    c.border_color = beautiful.border_focus
end)

client.connect_signal("unfocus", function(c)
    c.border_color = beautiful.border_normal
end)