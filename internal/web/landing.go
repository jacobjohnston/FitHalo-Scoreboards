package web

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/qr"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
)

// AppLink returns the invite as FitHalo opens it, which the QR code holds: the board's address and code.
func (s *Site) AppLink(board scoreboard.Board) string {
	query := url.Values{"url": {s.PublicURL.String()}, "code": {board.InviteCode}}
	return "fithalo://scoreboard?" + query.Encode()
}

// InviteLink returns the invite as a shareable https link, which opens the landing page.
func (s *Site) InviteLink(board scoreboard.Board) string {
	return s.PublicURL.String() + "/join/" + url.PathEscape(board.InviteCode)
}

// inviteQR returns the app link's QR code as inline SVG, with title for screen readers.
func (s *Site) inviteQR(board scoreboard.Board, title string) (template.HTML, error) {
	code, err := qr.Encode([]byte(s.AppLink(board)))
	if err != nil {
		return "", err
	}
	return template.HTML(code.SVG(title)), nil
}

// landing serves the board's own address.
func (s *Site) landing(writer http.ResponseWriter, request *http.Request) {
	s.renderLanding(writer, request, "", false)
}

// join serves an invite link.
func (s *Site) join(writer http.ResponseWriter, request *http.Request) {
	s.renderLanding(writer, request, request.PathValue("code"), true)
}

// renderLanding draws the landing page, with the invite on a current link or when the admin shows it.
func (s *Site) renderLanding(writer http.ResponseWriter, request *http.Request, code string, fromInviteLink bool) {
	page := s.newView(request, "landing", "Landing_Title")
	board := page.Board
	message := board.LandingMessage
	if message == "" {
		message = defaultMessage(page)
	}
	page.Title = message
	page.Data["Message"] = message
	page.Data["PasswordRequired"] = board.PasswordHash != ""

	codeIsCurrent := fromInviteLink && subtle.ConstantTimeCompare([]byte(code), []byte(board.InviteCode)) == 1
	status := http.StatusOK
	switch {
	case fromInviteLink && !codeIsCurrent:
		page.Data["Expired"] = true
		status = http.StatusNotFound
	case codeIsCurrent || board.ShowInvite:
		accepting, err := s.Service.AcceptingRequests(request.Context(), board)
		if err != nil {
			s.fail(writer, request, err)
			return
		}
		if !accepting {
			page.Data["NotAccepting"] = true
			break
		}
		svg, err := s.inviteQR(board, page.T("Landing_QRLabel"))
		if err != nil {
			s.fail(writer, request, err)
			return
		}
		page.Data["QR"] = svg
		page.Data["AppLink"] = template.URL(s.AppLink(board))
	}
	writer.Header().Set("Cache-Control", "no-store")
	s.render(writer, status, "landing", page)
}

// defaultMessage invites to the board by name, saying FitHalo Scoreboard only once however it is named.
func defaultMessage(page *view) string {
	name := page.Board.Name
	plain := strings.NewReplacer(" ", "", "-", "")
	normalized := plain.Replace(strings.ToLower(name))
	product := []string{plain.Replace(strings.ToLower(page.T("Common_UnnamedBoard"))), "fithaloscoreboard"}
	if name == "" || normalized == product[0] || normalized == product[1] {
		return page.T("Landing_DefaultMessageUnnamed")
	}
	if strings.Contains(normalized, product[0]) || strings.Contains(normalized, product[1]) {
		return page.T("Landing_DefaultMessageFullName", "name", name)
	}
	return page.T("Landing_DefaultMessage", "name", name)
}
