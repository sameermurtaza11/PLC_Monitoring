package web

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
)

// Account page: login form when logged out, user details + logout when in.
func (s *Server) accountPage(c *gin.Context) {
	s.renderAccount(c, http.StatusOK, "", c.Query("next"))
}

func (s *Server) login(c *gin.Context) {
	username := c.PostForm("username")
	next := c.PostForm("next")
	token, expires, err := s.auth.Login(c.Request.Context(), username, c.PostForm("password"), c.ClientIP())
	switch {
	case err == nil:
		auth.SetCookie(c, token, expires)
		log.Printf("web: login %q from %s", username, c.ClientIP())
		c.Redirect(http.StatusSeeOther, safeNext(next))
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrLocked):
		log.Printf("web: login failed for %q from %s: %v", username, c.ClientIP(), err)
		s.renderAccount(c, http.StatusUnauthorized, err.Error(), next)
	default:
		log.Printf("web: login error: %v", err)
		s.renderAccount(c, http.StatusInternalServerError, "login is unavailable, try again", next)
	}
}

func (s *Server) logout(c *gin.Context) {
	if token, err := c.Cookie(auth.CookieName); err == nil {
		if err := s.auth.Logout(c.Request.Context(), token); err != nil {
			log.Printf("web: logout: %v", err)
		}
	}
	auth.ClearCookie(c)
	c.Redirect(http.StatusSeeOther, "/login")
}

func (s *Server) renderAccount(c *gin.Context, status int, msg, next string) {
	next = safeNext(next)
	if next == "/" {
		next = ""
	}
	c.HTML(status, "login.html", s.pageData(c, "login", gin.H{"Error": msg, "Next": next}))
}
