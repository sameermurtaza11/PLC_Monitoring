package web

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/access"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// registerAccessAdmin adds the Access page. Its rules (administrators only,
// always a login) are in access.Rules, not switchable from the page itself.
func (s *Server) registerAccessAdmin(r *gin.Engine) {
	r.GET("/admin/access", s.accessPage)
	r.POST("/admin/access", auth.RequireCSRF(), s.accessSave)
}

// matrixCell is one checkbox of the role matrix.
type matrixCell struct {
	Role    string
	Allowed bool
	Locked  bool // fixed on: the administrator can never lose this feature
}

type matrixRow struct {
	Key         string
	Description string
	Cells       []matrixCell
}

type pageRow struct {
	access.Page
	RequiresLogin bool
}

func (s *Server) accessPage(c *gin.Context) {
	ctx := c.Request.Context()
	flags, err1 := s.store.LoadPageFlags(ctx)
	roles, err2 := s.store.ListRoles(ctx)
	perms, err3 := s.store.ListPermissions(ctx)
	matrix, err4 := s.store.LoadMatrix(ctx)
	for _, err := range []error{err1, err2, err3, err4} {
		if err != nil {
			log.Printf("web: access page: %v", err)
			c.String(http.StatusInternalServerError, "database error")
			return
		}
	}

	pages := make([]pageRow, 0, len(access.Pages))
	for _, p := range access.Pages {
		v, ok := flags[p.Key]
		pages = append(pages, pageRow{Page: p, RequiresLogin: v || !ok}) // missing = protected
	}
	rows := make([]matrixRow, 0, len(perms))
	for _, p := range perms {
		row := matrixRow{Key: p.Key, Description: p.Description}
		for _, r := range roles {
			locked := access.IsLocked(r.Name, p.Key)
			row.Cells = append(row.Cells, matrixCell{Role: r.Name, Allowed: matrix[r.Name][p.Key] || locked, Locked: locked})
		}
		rows = append(rows, row)
	}

	msg := ""
	if c.Query("ok") == "1" {
		msg = "Saved. The new settings apply to the next request of every user."
	}
	c.HTML(http.StatusOK, "access.html", s.pageData(c, "access", gin.H{
		"Pages": pages, "Roles": roles, "Rows": rows, "Msg": msg,
	}))
}

// accessSave stores both switches from one form:
//
//	login=<page key>      repeated: pages whose authentication switch is ON
//	allow=<role>|<feature> repeated: role/feature boxes that are ticked
//
// A page or box that is not in the form is OFF. The administrator keeps
// access.manage and users.manage whatever was submitted.
func (s *Server) accessSave(c *gin.Context) {
	user := auth.FromContext(c)
	actor := store.Actor{ID: user.ID, Username: user.Username, IP: c.ClientIP()}

	on := map[string]bool{}
	for _, k := range c.PostFormArray("login") {
		on[k] = true
	}
	flags := map[string]bool{}
	for _, p := range access.Pages {
		flags[p.Key] = on[p.Key]
	}

	matrix := store.Matrix{}
	for _, pair := range c.PostFormArray("allow") {
		role, perm, ok := strings.Cut(pair, "|")
		if !ok || role == "" || perm == "" {
			continue
		}
		if matrix[role] == nil {
			matrix[role] = map[string]bool{}
		}
		matrix[role][perm] = true
	}
	matrix = access.EnforceGuardrails(matrix)

	ctx := c.Request.Context()
	if err := s.store.SavePageFlags(ctx, flags, actor); err != nil {
		log.Printf("web: save page flags: %v", err)
		c.String(http.StatusInternalServerError, "could not save the page settings")
		return
	}
	if err := s.store.SaveMatrix(ctx, matrix, actor); err != nil {
		log.Printf("web: save role matrix: %v", err)
		c.String(http.StatusInternalServerError, "could not save the role settings")
		return
	}
	s.pages.Reload() // the next request already uses the new switches
	c.Redirect(http.StatusSeeOther, "/admin/access?ok=1")
}
