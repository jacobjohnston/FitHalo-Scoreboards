package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/admin"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

const (
	// dateLayout is how the portal writes dates.
	dateLayout = "2006-01-02"

	// shortIDLength is how many characters of a blocked member's identifier the portal shows.
	shortIDLength = 6

	// kilobyte is the unit the Details page counts sizes in.
	kilobyte = 1024
)

// notices map the keys a redirect may name to string keys, so a query cannot put any text on a page.
var notices = map[string]string{
	"saved": "Notice_Saved", "inviteReplaced": "Notice_InviteReplaced", "approved": "Notice_Approved",
	"deleted": "Notice_Deleted", "blocked": "Notice_Blocked", "unblocked": "Notice_Unblocked",
	"avatarCleared": "Notice_AvatarCleared", "passwordSet": "Notice_BoardPasswordSet",
	"passwordRemoved": "Notice_BoardPasswordRemoved", "passwordChanged": "Notice_PasswordChanged",
	"passwordOn": "Notice_PasswordOn", "passwordOff": "Notice_PasswordOff", "passkeyAdded": "Notice_PasskeyAdded",
	"passkeyRenamed": "Notice_PasskeyRenamed", "passkeyRemoved": "Notice_PasskeyRemoved", "recovered": "Notice_Recovered",
}

// adminView returns a signed-in page's view, with any notice the redirect asked for.
func (s *Site) adminView(request *http.Request, page, titleKey string, session admin.Session) *view {
	result := s.newView(request, page, titleKey)
	result.SignedIn = true
	result.CSRF = session.CSRF
	if key, known := notices[request.URL.Query().Get("done")]; known {
		result.Notice = result.T(key)
	}
	if count, err := s.Service.MemberCount(request.Context()); err == nil {
		result.MemberLimitReached = count >= scoreboard.MaximumMembers
	}
	return result
}

// adminHome sends the admin to setup until the board is set up, and to the Board page after.
func (s *Site) adminHome(writer http.ResponseWriter, request *http.Request) {
	setUp, err := s.Admin.IsSetUp(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	if !setUp {
		http.Redirect(writer, request, s.basePath()+"/admin/setup", http.StatusSeeOther)
		return
	}
	http.Redirect(writer, request, s.basePath()+"/admin/board", http.StatusSeeOther)
}

// Setup

// setupPage shows the setup form, until the board is set up.
func (s *Site) setupPage(writer http.ResponseWriter, request *http.Request) {
	if setUp, err := s.Admin.IsSetUp(request.Context()); err != nil || setUp {
		http.Redirect(writer, request, s.basePath()+"/admin/sign-in", http.StatusSeeOther)
		return
	}
	s.render(writer, http.StatusOK, "setup", s.newView(request, "setup", "Setup_Title"))
}

// setup names the board, creates the admin with the setup code, and signs them in.
func (s *Site) setup(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, formBodyLimit)
	page := s.newView(request, "setup", "Setup_Title")
	boardName := strings.TrimSpace(request.PostFormValue("boardName"))
	username := request.PostFormValue("username")
	page.Data["BoardName"], page.Data["Username"] = boardName, username
	newPassword := request.PostFormValue("password")

	failure := ""
	switch {
	case utf8.RuneCountInString(boardName) == 0 || utf8.RuneCountInString(boardName) > scoreboard.MaximumBoardName:
		failure = "Error_BoardNameLength"
	case newPassword != request.PostFormValue("confirm"):
		failure = "Error_PasswordMismatch"
	}
	if failure == "" {
		switch err := s.Admin.Setup(request.Context(), request.PostFormValue("code"), username, newPassword); {
		case err == nil:
		case errors.Is(err, admin.ErrWrongCode):
			failure = "Error_WrongSetupCode"
		case errors.Is(err, admin.ErrInvalidUsername):
			failure = "Error_UsernameLength"
		case errors.Is(err, admin.ErrInvalidPassword):
			failure = "Error_PasswordLength"
		case errors.Is(err, admin.ErrBreachedPassword):
			failure = "Error_PasswordBreached"
		case errors.Is(err, admin.ErrAlreadySetUp):
			http.Redirect(writer, request, s.basePath()+"/admin/sign-in", http.StatusSeeOther)
			return
		default:
			s.fail(writer, request, err)
			return
		}
	}
	if failure != "" {
		page.Error = page.T(failure)
		s.render(writer, http.StatusBadRequest, "setup", page)
		return
	}
	if _, err := s.Service.UpdateBoard(request.Context(), func(board *scoreboard.Board) error {
		board.Name = boardName
		return nil
	}); err != nil {
		s.fail(writer, request, err)
		return
	}
	s.startSession(writer, request)
}

// Signing in and out

// signInPage shows the sign-in form, or sends a board not yet set up to setup.
func (s *Site) signInPage(writer http.ResponseWriter, request *http.Request) {
	if setUp, err := s.Admin.IsSetUp(request.Context()); err == nil && !setUp {
		http.Redirect(writer, request, s.basePath()+"/admin/setup", http.StatusSeeOther)
		return
	}
	page := s.newView(request, "signin", "SignIn_Title")
	page.Script = true
	if key, known := notices[request.URL.Query().Get("done")]; known {
		page.Notice = page.T(key)
	}
	s.render(writer, http.StatusOK, "signin", page)
}

// signIn checks the admin's username and password and starts a session.
func (s *Site) signIn(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, formBodyLimit)
	username := request.PostFormValue("username")
	err := s.Admin.SignIn(request.Context(), username, request.PostFormValue("password"))
	if err == nil {
		s.startSession(writer, request)
		return
	}
	failure := "Error_WrongSignIn"
	switch {
	case errors.Is(err, admin.ErrPasswordOff):
		failure = "Error_PasswordSignInOff"
	case errors.Is(err, admin.ErrNotSetUp):
		http.Redirect(writer, request, s.basePath()+"/admin/setup", http.StatusSeeOther)
		return
	case errors.Is(err, password.ErrPepperChanged):
		if s.Logger != nil {
			s.Logger.Error("the admin's password was hashed with a pepper this server does not have; recover the admin to set a new one")
		}
	case !errors.Is(err, admin.ErrWrongCredentials):
		s.fail(writer, request, err)
		return
	}
	page := s.newView(request, "signin", "SignIn_Title")
	page.Script = true
	page.Data["Username"] = username
	page.Error = page.T(failure)
	s.render(writer, http.StatusUnauthorized, "signin", page)
}

// startSession sets a new session's cookie and opens the Board page.
func (s *Site) startSession(writer http.ResponseWriter, request *http.Request) {
	token, _, err := s.Admin.NewSession(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.setCookie(writer, sessionCookie, token, admin.SessionLifetime)
	http.Redirect(writer, request, s.basePath()+"/admin/board", http.StatusSeeOther)
}

// signOut ends the session and clears its cookie.
func (s *Site) signOut(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	if err := s.Admin.EndSession(request.Context(), sessionToken(request)); err != nil {
		s.fail(writer, request, err)
		return
	}
	s.setCookie(writer, sessionCookie, "", 0)
	http.Redirect(writer, request, s.basePath()+"/admin/sign-in", http.StatusSeeOther)
}

// Recovery

// recoverPage shows the recovery form.
func (s *Site) recoverPage(writer http.ResponseWriter, request *http.Request) {
	s.render(writer, http.StatusOK, "recover", s.newView(request, "recover", "Recover_Title"))
}

// recover sets a new password with a recovery code from reset-admin.
func (s *Site) recover(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, formBodyLimit)
	page := s.newView(request, "recover", "Recover_Title")
	newPassword := request.PostFormValue("password")
	failure := ""
	if newPassword != request.PostFormValue("confirm") {
		failure = "Error_PasswordMismatch"
	} else {
		switch err := s.Admin.Recover(request.Context(), request.PostFormValue("code"), newPassword); {
		case err == nil:
			http.Redirect(writer, request, s.basePath()+"/admin/sign-in?done=recovered", http.StatusSeeOther)
			return
		case errors.Is(err, admin.ErrWrongCode):
			failure = "Error_WrongRecoveryCode"
		case errors.Is(err, admin.ErrInvalidPassword):
			failure = "Error_PasswordLength"
		case errors.Is(err, admin.ErrBreachedPassword):
			failure = "Error_PasswordBreached"
		case errors.Is(err, admin.ErrNotSetUp):
			http.Redirect(writer, request, s.basePath()+"/admin/setup", http.StatusSeeOther)
			return
		default:
			s.fail(writer, request, err)
			return
		}
	}
	page.Error = page.T(failure)
	s.render(writer, http.StatusBadRequest, "recover", page)
}

// The board

// boardPage shows the Board page.
func (s *Site) boardPage(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	s.renderBoard(writer, request, session, http.StatusOK, "")
}

// renderBoard draws the Board page with its invite, and failure's text when it is set.
func (s *Site) renderBoard(writer http.ResponseWriter, request *http.Request, session admin.Session, status int, failure string) {
	page := s.adminView(request, "board", "Nav_Board", session)
	if failure != "" {
		page.Error = page.T(failure)
	}
	svg, err := s.inviteQR(page.Board, page.T("Landing_QRLabel"))
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	page.Data["QR"] = svg
	page.Data["InviteLink"] = s.InviteLink(page.Board)
	page.Data["DefaultMessage"] = defaultMessage(page)
	if page.Board.HasAvatar {
		// The avatar's address names its content, so a new one shows at once despite shared caching.
		if image, err := s.Service.BoardAvatar(request.Context()); err == nil {
			digest := sha256.Sum256(image)
			page.Data["AvatarVersion"] = hex.EncodeToString(digest[:versionBytes])
		}
	}
	s.render(writer, status, "board", page)
}

// saveBoard saves the board's name, avatar, description, landing message and invite visibility. An
// image that does not decode saves nothing.
func (s *Site) saveBoard(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	name := strings.TrimSpace(request.PostFormValue("name"))
	description := strings.TrimSpace(request.PostFormValue("description"))
	message := strings.TrimSpace(request.PostFormValue("landingMessage"))
	switch {
	case utf8.RuneCountInString(name) == 0 || utf8.RuneCountInString(name) > scoreboard.MaximumBoardName:
		s.renderBoard(writer, request, session, http.StatusBadRequest, "Error_BoardNameLength")
		return
	case utf8.RuneCountInString(description) > scoreboard.MaximumDescription:
		s.renderBoard(writer, request, session, http.StatusBadRequest, "Error_DescriptionLength")
		return
	case utf8.RuneCountInString(message) > scoreboard.MaximumLandingMessage:
		s.renderBoard(writer, request, session, http.StatusBadRequest, "Error_MessageLength")
		return
	}
	image, err := uploadedFile(request, "avatar")
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	if image != nil {
		defer image.Close()
	}
	switch {
	case image != nil:
		var contentType string
		if contentType, err = sniff(image); err == nil {
			err = s.Service.SetBoardAvatar(request.Context(), image, contentType)
		}
	case request.PostFormValue("removeAvatar") == "on":
		err = s.Service.RemoveBoardAvatar(request.Context())
	}
	var failure *problem.Problem
	if errors.As(err, &failure) && failure.Code == problem.ImageInvalid {
		s.renderBoard(writer, request, session, http.StatusBadRequest, "Error_ImageInvalid")
		return
	}
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	_, err = s.Service.UpdateBoard(request.Context(), func(board *scoreboard.Board) error {
		board.Name = name
		board.Description = description
		board.LandingMessage = message
		board.ShowInvite = request.PostFormValue("showInvite") == "on"
		return nil
	})
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "board", "saved")
}

// uploadedFile opens the file a multipart form posted as field, or returns nil when none was chosen.
func uploadedFile(request *http.Request, field string) (multipart.File, error) {
	file, header, err := request.FormFile(field)
	if errors.Is(err, http.ErrMissingFile) || errors.Is(err, http.ErrNotMultipart) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if header.Size == 0 {
		file.Close()
		return nil, nil
	}
	return file, nil
}

// sniff returns the content type of file's first bytes and leaves it at the start.
func sniff(file multipart.File) (string, error) {
	head := make([]byte, 512)
	read, err := file.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return http.DetectContentType(head[:read]), nil
}

// replaceInvite issues a new invite code, so the old one stops working.
func (s *Site) replaceInvite(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	if _, err := s.Service.ReplaceInvite(request.Context()); err != nil {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "board", "inviteReplaced")
}

// inviteSVG downloads the invite's QR code as an SVG file.
func (s *Site) inviteSVG(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	board, err := s.Service.Board(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	svg, err := s.inviteQR(board, board.Name)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	writer.Header().Set("Content-Type", "image/svg+xml")
	writer.Header().Set("Content-Disposition", `attachment; filename="scoreboard-invite.svg"`)
	writer.Write([]byte(svg))
}

// Members

// A memberRow is one member on the Members page.
type memberRow struct {
	// ID is the member's identifier.
	ID string

	// Name is the member's display name.
	Name string

	// Joined says when the member joined.
	Joined string

	// Sharing describes the member's effective sharing.
	Sharing string

	// HasAvatar reports whether the member has an avatar to clear.
	HasAvatar bool
}

// A blockedRow is one blocked member on the Members page.
type blockedRow struct {
	// ID is the blocked member's identifier.
	ID string

	// ShortID names the blocked member by the start of their identifier.
	ShortID string

	// When says when the member was blocked.
	When string
}

// membersPage lists members waiting for approval, active members, and blocked members.
func (s *Site) membersPage(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	page := s.adminView(request, "members", "Nav_Members", session)
	members, err := s.Service.Members(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	var pending, active []memberRow
	for _, member := range members {
		row := memberRow{
			ID:        member.ID,
			Name:      member.DisplayName,
			Joined:    page.T("Members_Joined", "date", member.JoinedAt.Format(dateLayout)),
			Sharing:   describeSharing(page, scoreboard.Effective(member, page.Board)),
			HasAvatar: member.HasAvatar,
		}
		if member.Status == scoreboard.StatusPending {
			pending = append(pending, row)
		} else {
			active = append(active, row)
		}
	}
	blocked, err := s.Service.Blocked(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	var blockedRows []blockedRow
	for _, entry := range blocked {
		blockedRows = append(blockedRows, blockedRow{
			ID:      entry.ID,
			ShortID: page.T("Members_BlockedMember", "id", entry.ID[:shortIDLength]),
			When:    page.T("Members_BlockedOn", "date", entry.BlockedAt.Format(dateLayout)),
		})
	}
	page.Data["Pending"], page.Data["Active"], page.Data["Blocked"] = pending, active, blockedRows
	page.Data["PendingLimit"] = page.Board.PendingLimit()
	s.render(writer, http.StatusOK, "members", page)
}

// describeSharing names a member's effective sharing in a few words.
func describeSharing(page *view, effective sharing.Sharing) string {
	parts := []string{page.T("Sharing_Score_" + string(effective.Score))}
	if effective.Score == sharing.ScoreDetails && effective.Activity != sharing.ActivityNone {
		parts = append(parts, page.T("Sharing_Activity_"+string(effective.Activity)))
	}
	if effective.Routes {
		parts = append(parts, page.T("Sharing_Routes"))
	}
	if effective.StrengthWeights {
		parts = append(parts, page.T("Sharing_StrengthWeights"))
	} else if effective.StrengthSets {
		parts = append(parts, page.T("Sharing_StrengthSets"))
	}
	return strings.Join(parts, ", ")
}

// A usageRow is one weekly challenge on a member's Details page.
type usageRow struct {
	// Week names the weekly challenge by the week it starts.
	Week string

	// Detail gives the weekly challenge's size and its routes' count and size.
	Detail string

	// Percent is how much of the limit for one weekly challenge it uses, from 0 to 100.
	Percent int

	// Share says Percent in words.
	Share string
}

// detailsPage shows whether one member may approve requests, and what they store for each weekly challenge against the
// limit for one.
func (s *Site) detailsPage(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	id := request.PathValue("id")
	if !scoreboard.IsIdentifier(id) {
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
		return
	}
	ctx := request.Context()
	member, err := s.Service.Member(ctx, id)
	if isNotFound(err) {
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
		return
	}
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	usage, err := s.Service.Usage(ctx, id)
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	page := s.adminView(request, "details", "Details_Title", session)
	size := func(bytes int) string {
		return page.T("Usage_Kilobytes", "count", (bytes+kilobyte-1)/kilobyte)
	}
	rows := make([]usageRow, 0, len(usage.Challenges))
	for _, challenge := range usage.Challenges {
		percent := min(100, (challenge.Total()*100+scoreboard.MaximumWeeklyChallengeBytes-1)/scoreboard.MaximumWeeklyChallengeBytes)
		rows = append(rows, usageRow{
			Week: page.T("Usage_Week", "date", challenge.WeekStart),
			Detail: page.T("Usage_WeekDetail", "challenge", size(challenge.ChallengeBytes), "count", challenge.Routes,
				"routes", size(challenge.RouteBytes)),
			Percent: percent,
			Share:   page.T("Usage_Share", "percent", percent),
		})
	}
	avatar := page.T("Usage_NoAvatar")
	if usage.AvatarBytes > 0 {
		avatar = page.T("Usage_Avatar", "size", size(usage.AvatarBytes))
	}
	page.Data["CanApproveMembers"] = member.CanApproveMembers
	page.Data["ID"] = member.ID
	page.Data["IsActive"] = member.Status == scoreboard.StatusActive
	page.Data["Joined"] = page.T("Members_Joined", "date", member.JoinedAt.Format(dateLayout))
	page.Data["Name"] = member.DisplayName
	page.Data["Sharing"] = describeSharing(page, scoreboard.Effective(member, page.Board))
	page.Data["Summary"] = page.T("Usage_Summary", "size", size(usage.Total()), "count", len(usage.Challenges))
	page.Data["Avatar"] = avatar
	page.Data["Challenges"] = rows
	s.render(writer, http.StatusOK, "details", page)
}

// setCanApprove saves the Details page's Can Approve Members checkbox.
func (s *Site) setCanApprove(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	id := request.PathValue("id")
	if !scoreboard.IsIdentifier(id) {
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
		return
	}
	err := s.Service.SetCanApproveMembers(request.Context(), id, request.PostFormValue("canApproveMembers") == "on")
	if isNotFound(err) {
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
		return
	}
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "members/"+id+"/details", "saved")
}

// memberAction approves, deletes, blocks, or clears the avatar of a member.
func (s *Site) memberAction(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	id := request.PathValue("id")
	if !scoreboard.IsIdentifier(id) {
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
		return
	}
	ctx := request.Context()
	var err error
	notice := ""
	switch request.PathValue("action") {
	case "approve":
		err, notice = s.Service.SetStatus(ctx, id, scoreboard.StatusActive), "approved"
	case "delete":
		err, notice = s.Service.DeleteMember(ctx, id, false), "deleted"
	case "block":
		err, notice = s.Service.DeleteMember(ctx, id, true), "blocked"
	case "clear-avatar":
		err, notice = s.Service.DeleteAvatar(ctx, id), "avatarCleared"
	default:
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
		return
	}
	if err != nil && !isNotFound(err) {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "members", notice)
}

// unblock lets a blocked member ask to join again.
func (s *Site) unblock(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	id := request.PathValue("id")
	if !scoreboard.IsIdentifier(id) {
		s.message(writer, request, http.StatusNotFound, "Error_NotFoundTitle", "Error_NotFound")
		return
	}
	if err := s.Service.Unblock(request.Context(), id); err != nil {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "members", "unblocked")
}

// Rules

// rulesPage shows the Rules page.
func (s *Site) rulesPage(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	s.renderRules(writer, request, session, http.StatusOK, "")
}

// renderRules draws the Rules page with the stored choices, and failure's text when it is set.
func (s *Site) renderRules(writer http.ResponseWriter, request *http.Request, session admin.Session, status int, failure string) {
	page := s.adminView(request, "rules", "Nav_Rules", session)
	if failure != "" {
		page.Error = page.T(failure)
	}
	maximum := page.Board.MaximumSharing
	var scores, activities []option
	for _, level := range []sharing.ScoreLevel{sharing.ScoreProgress, sharing.ScoreScore, sharing.ScoreDetails} {
		scores = append(scores, option{Value: string(level), Label: page.T("Sharing_Score_" + string(level)), Selected: maximum.Score == level})
	}
	for _, level := range []sharing.ActivityLevel{sharing.ActivityNone, sharing.ActivityPoints, sharing.ActivitySummary, sharing.ActivityFull} {
		activities = append(activities, option{Value: string(level), Label: page.T("Sharing_Activity_" + string(level)), Selected: maximum.Activity == level})
	}
	page.Data["ScoreLevels"], page.Data["ActivityLevels"] = scores, activities
	page.Data["PendingLimit"] = page.Board.PendingLimit()
	page.Data["PasswordSet"] = page.Board.PasswordHash != ""
	var weekdays []option
	for _, day := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
		weekdays = append(weekdays, option{Value: day, Label: page.T("Weekday_" + day), Selected: page.Board.WeekStartDay == day})
	}
	page.Data["Weekdays"] = weekdays
	s.render(writer, status, "rules", page)
}

// saveRules saves the board's rules, and erases its history when the weekly challenge start day changes.
func (s *Site) saveRules(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	activity := sharing.ActivityLevel(request.PostFormValue("activity"))
	if activity == "" {
		// The page disables the activity choice below score details, and a disabled field is not sent.
		activity = sharing.ActivityNone
	}
	maximum := sharing.Sharing{
		Score:           sharing.ScoreLevel(request.PostFormValue("score")),
		Activity:        activity,
		Routes:          request.PostFormValue("routes") == "on",
		StrengthSets:    request.PostFormValue("strengthSets") == "on",
		StrengthWeights: request.PostFormValue("strengthWeights") == "on",
	}
	retention, retentionErr := strconv.Atoi(request.PostFormValue("retentionWeeks"))
	maximumPending, pendingErr := strconv.Atoi(request.PostFormValue("maximumPending"))
	joinMode, ranking := request.PostFormValue("joinMode"), request.PostFormValue("ranking")
	weekStartDay := request.PostFormValue("weekStartDay")
	switch {
	case maximum.Validate() != nil || (joinMode != scoreboard.JoinOpen && joinMode != scoreboard.JoinApproval) ||
		(ranking != scoreboard.RankByProgress && ranking != scoreboard.RankByName) || !scoreboard.IsWeekStartDay(weekStartDay):
		s.renderRules(writer, request, session, http.StatusBadRequest, "Error_Failed")
		return
	case retentionErr != nil || retention < scoreboard.MinimumRetentionWeeks || retention > scoreboard.MaximumRetentionWeeks:
		s.renderRules(writer, request, session, http.StatusBadRequest, "Error_RetentionRange")
		return
	case pendingErr != nil || maximumPending < scoreboard.MinimumPending || maximumPending > scoreboard.MaximumPendingLimit:
		s.renderRules(writer, request, session, http.StatusBadRequest, "Error_MaximumPendingRange")
		return
	}
	weekStartChanged := false
	_, err := s.Service.UpdateBoard(request.Context(), func(board *scoreboard.Board) error {
		board.JoinMode = joinMode
		board.AvatarsAllowed = request.PostFormValue("avatarsAllowed") == "on"
		board.MaximumSharing = maximum.Normalized()
		board.Ranking = ranking
		board.RetentionWeeks = retention
		board.MaximumPending = maximumPending
		weekStartChanged = board.WeekStartDay != weekStartDay
		board.WeekStartDay = weekStartDay
		return nil
	})
	if err == nil && weekStartChanged {
		err = s.Service.EraseHistory(request.Context())
	}
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "rules", "saved")
}

// saveBoardPassword sets or removes the password members need to join.
func (s *Site) saveBoardPassword(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	if request.PostFormValue("action") == "remove" {
		if err := s.Service.SetBoardPassword(request.Context(), ""); err != nil {
			s.fail(writer, request, err)
			return
		}
		s.redirect(writer, request, "rules", "passwordRemoved")
		return
	}
	newPassword := request.PostFormValue("password")
	if scoreboard.ValidatePassword(newPassword) != nil {
		s.renderRules(writer, request, session, http.StatusBadRequest, "Error_BoardPasswordLength")
		return
	}
	if err := s.Service.SetBoardPassword(request.Context(), newPassword); err != nil {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "rules", "passwordSet")
}

// Account

// A passkeyRow is one passkey on the Account page.
type passkeyRow struct {
	// ID is the passkey's credential identifier, in unpadded base64url.
	ID string

	// Name is the admin's name for the passkey.
	Name string

	// Detail says when the passkey was added and last used.
	Detail string
}

// accountPage shows the Account page.
func (s *Site) accountPage(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	s.renderAccount(writer, request, session, http.StatusOK, "")
}

// renderAccount draws the Account page with the passkeys, and failure's text when it is set.
func (s *Site) renderAccount(writer http.ResponseWriter, request *http.Request, session admin.Session, status int, failure string) {
	page := s.adminView(request, "account", "Nav_Account", session)
	page.Script = true
	if failure != "" {
		page.Error = page.T(failure)
	}
	account, err := s.Admin.Account(request.Context())
	if err != nil {
		s.fail(writer, request, err)
		return
	}
	var rows []passkeyRow
	for _, passkey := range account.Passkeys {
		detail := page.T("Account_PasskeyAdded", "date", passkey.CreatedAt.Format(dateLayout))
		if !passkey.LastUsedAt.IsZero() {
			detail += " · " + page.T("Account_PasskeyUsed", "date", passkey.LastUsedAt.Format(dateLayout))
		}
		rows = append(rows, passkeyRow{ID: passkey.ID, Name: passkey.Name, Detail: detail})
	}
	page.Data["Passkeys"] = rows
	page.Data["PasswordEnabled"] = account.PasswordEnabled
	if account.PasswordEnabled && account.PasswordBreached && page.Error == "" {
		page.Error = page.T("Account_PasswordBreached")
	}
	page.Data["Username"] = account.Username
	s.render(writer, status, "account", page)
}

// changePassword replaces the admin's password, given the current one, and signs out every other browser.
func (s *Site) changePassword(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	newPassword := request.PostFormValue("password")
	if newPassword != request.PostFormValue("confirm") {
		s.renderAccount(writer, request, session, http.StatusBadRequest, "Error_PasswordMismatch")
		return
	}
	current := request.PostFormValue("current")
	switch err := s.Admin.ChangePassword(request.Context(), sessionToken(request), current, newPassword); {
	case err == nil:
		s.redirect(writer, request, "account", "passwordChanged")
	case errors.Is(err, admin.ErrWrongCredentials):
		s.renderAccount(writer, request, session, http.StatusBadRequest, "Error_WrongCurrentPassword")
	case errors.Is(err, admin.ErrInvalidPassword):
		s.renderAccount(writer, request, session, http.StatusBadRequest, "Error_PasswordLength")
	case errors.Is(err, admin.ErrBreachedPassword):
		s.renderAccount(writer, request, session, http.StatusBadRequest, "Error_PasswordBreached")
	default:
		s.fail(writer, request, err)
	}
}

// setPasswordEnabled turns password sign-in on or off; off needs a passkey.
func (s *Site) setPasswordEnabled(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	enabled := request.PostFormValue("enabled") == "on"
	switch err := s.Admin.SetPasswordEnabled(request.Context(), enabled); {
	case err == nil && enabled:
		s.redirect(writer, request, "account", "passwordOn")
	case err == nil:
		s.redirect(writer, request, "account", "passwordOff")
	case errors.Is(err, admin.ErrNoPasskey):
		s.renderAccount(writer, request, session, http.StatusBadRequest, "Error_NeedPasskey")
	default:
		s.fail(writer, request, err)
	}
}

// renamePasskey renames one of the admin's passkeys.
func (s *Site) renamePasskey(writer http.ResponseWriter, request *http.Request, _ admin.Session) {
	if err := s.Admin.RenamePasskey(request.Context(), request.PathValue("id"), request.PostFormValue("name")); err != nil {
		s.fail(writer, request, err)
		return
	}
	s.redirect(writer, request, "account", "passkeyRenamed")
}

// deletePasskey removes one of the admin's passkeys, unless it is the last while the password is off,
// and signs out every other browser.
func (s *Site) deletePasskey(writer http.ResponseWriter, request *http.Request, session admin.Session) {
	switch err := s.Admin.RemovePasskey(request.Context(), sessionToken(request), request.PathValue("id")); {
	case err == nil:
		s.redirect(writer, request, "account", "passkeyRemoved")
	case errors.Is(err, admin.ErrLastPasskey):
		s.renderAccount(writer, request, session, http.StatusBadRequest, "Error_LastPasskey")
	default:
		s.fail(writer, request, err)
	}
}

// isNotFound reports whether err means the member is already gone.
func isNotFound(err error) bool {
	var failure *problem.Problem
	return errors.Is(err, store.ErrNotFound) || (errors.As(err, &failure) && failure.Code == problem.NotFound)
}
