package functional

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/viewer_privacy",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "An owner, a contributor and a viewer on one shared datapack, over REST. The viewer lists, counts and opens every member's " +
			"conversations, their traces and every member's memories, but no response names the owner or the contributor (id or email). " +
			"The contributor still sees who started each conversation. The KB document list and activity log name the uploader to the " +
			"contributor and to nobody for the viewer. The viewer can't delete the owner's memory (404, not its own), and keep-as-memory and artifact uploads are 403.",
		Run: viewerPrivacy,
	})
}

func viewerPrivacy(t *core.T) error {
	accts, done := accountsFor(t, 3)
	defer done()
	var us [3]*user
	for i := range us {
		u, err := newUser(t, accts[i])
		defer u.close()
		if err != nil {
			return err
		}
		us[i] = u
	}
	owner, contributor, viewer := us[0], us[1], us[2]
	mk := "vp" + t.ResourceSuffix()

	dp, err := ownedDatapack(t, owner, "dp-viewer-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	if err := shareWith(t, owner, contributor, dp.ID, "contributor"); err != nil {
		return err
	}
	if err := shareWith(t, owner, viewer, dp.ID, "viewer"); err != nil {
		return err
	}

	// Each turn's message id is kept so the trace route can be asked for it.
	// A viewer can't start a conversation, so the owner and the contributor do.
	type started struct{ conv, message string }
	startConversation := func(u *user, who string) (started, error) {
		conv, err := u.conversation(dp.ID, who+" "+mk)
		if err != nil {
			return started{}, err
		}
		var r struct {
			MessageID string `json:"message_id"`
		}
		err = u.callJSON("conversation_add_message", map[string]any{
			"datapack_id": dp.ID, "conversation_id": conv,
			"input": fmt.Sprintf("%s asks %s: where do the release notes live?", who, mk), "output": "In the wiki.",
		}, &r)
		if err == nil && r.MessageID == "" {
			err = fmt.Errorf("%s's conversation_add_message answered no message_id", who)
		}
		return started{conv: conv, message: r.MessageID}, err
	}
	var ownerConv, contributorConv started
	if err := t.Step("owner_and_contributor_each_start_a_conversation", func(h *core.StepHandle) error {
		var err error
		if ownerConv, err = startConversation(owner, "owner"); err != nil {
			return fmt.Errorf("owner: %w", err)
		}
		if contributorConv, err = startConversation(contributor, "contributor"); err != nil {
			return fmt.Errorf("contributor: %w", err)
		}
		h.Set("owner_conversation", ownerConv.conv)
		h.Set("contributor_conversation", contributorConv.conv)
		return nil
	}); err != nil {
		return err
	}

	// owners are what a viewer's responses must never contain.
	owners := []string{owner.acct.ID, owner.acct.Email, contributor.acct.ID, contributor.acct.Email}
	namesNoOwner := func(what string, body []byte) error {
		for _, o := range owners {
			if o != "" && strings.Contains(string(body), o) {
				return fmt.Errorf("%s names %q, which a viewer isn't told: %s", what, o, truncateBody(body))
			}
		}
		return nil
	}
	norm := func(id string) string { return strings.ReplaceAll(id, "-", "") }
	type convList struct {
		Conversations []struct {
			ID    string `json:"id"`
			Email string `json:"user_email_id"`
		} `json:"conversations"`
	}
	listPath := "/datapacks/" + dp.ID + "/conversations?limit=200"
	listed := func(u *user) (convList, []byte, int, error) {
		var l convList
		r, err := u.api.Raw(t, get, listPath, nil)
		if err != nil {
			return l, nil, 0, err
		}
		if err := wantStatus("GET "+listPath, r, httpOK, ""); err != nil {
			return l, nil, 0, err
		}
		var env struct {
			Data convList `json:"data"`
		}
		if err := json.Unmarshal(r.Body, &env); err != nil {
			return l, nil, 0, fmt.Errorf("decode %s: %w", listPath, err)
		}
		l = env.Data
		var c struct {
			Count int `json:"count"`
		}
		if err := u.rest(get, "/datapacks/"+dp.ID+"/conversations/count", nil, httpOK, &c); err != nil {
			return l, nil, 0, err
		}
		return l, r.Body, c.Count, nil
	}
	has := func(l convList, id string) bool {
		for _, c := range l.Conversations {
			if norm(c.ID) == norm(id) {
				return true
			}
		}
		return false
	}

	var errs []error
	var contributorCount int
	if err := t.Step("contributor_lists_every_conversation_with_its_owner", func(h *core.StepHandle) error {
		var last convList
		err := eventually(t, "both conversations, with owners, in the contributor's list", visibleWithin(t), func() (bool, error) {
			l, _, count, err := listed(contributor)
			if err != nil {
				return false, err
			}
			last, contributorCount = l, count
			named := false
			for _, c := range l.Conversations {
				named = named || c.Email == owner.acct.Email
			}
			return has(l, ownerConv.conv) && has(l, contributorConv.conv) && count >= 2 && named, nil
		})
		h.Set("count", contributorCount)
		if err != nil {
			return fmt.Errorf("%w; contributor listed %+v", err, last)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("viewer_lists_every_conversation_without_owners", func(h *core.StepHandle) error {
		l, body, count, err := listed(viewer)
		if err != nil {
			return err
		}
		h.Set("count", count)
		if !has(l, ownerConv.conv) || !has(l, contributorConv.conv) || count != contributorCount {
			return fmt.Errorf("the viewer lists %+v (count %d), want both conversations and the contributor's count %d",
				l, count, contributorCount)
		}
		return namesNoOwner("the viewer's conversation list", body)
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("viewer_opens_a_conversation_and_its_traces_without_owners", func(*core.StepHandle) error {
		base := "/datapacks/" + dp.ID + "/conversations/" + ownerConv.conv
		for _, p := range []string{
			base,
			base + "/messages/" + ownerConv.message + "/trace",
			base + "/messages/" + ownerConv.message + "/decision-trace",
		} {
			var body []byte
			err := eventually(t, "the viewer GET "+p, visibleWithin(t), func() (bool, error) {
				r, err := viewer.api.Raw(t, get, p, nil)
				if err != nil {
					return false, err
				}
				body = r.Body
				return r.Status == httpOK, nil
			})
			if err != nil {
				return fmt.Errorf("%w: %s", err, truncateBody(body))
			}
			if err := namesNoOwner("the viewer's GET "+p, body); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	fact := fmt.Sprintf("The %s on-call rotation changes on Mondays.", mk)
	if err := t.Step("viewer_sees_every_memory_without_owners", func(h *core.StepHandle) error {
		var added struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if err := owner.callJSON("memory_add", map[string]any{
			"datapack_id": dp.ID, "conversation_id": ownerConv.conv, "text": fact,
		}, &added); err != nil {
			return err
		}
		// Fail rather than skip the delete check below.
		if len(added.Results) == 0 {
			return fmt.Errorf("the owner's memory_add returned no memory id")
		}
		type count struct {
			Count int `json:"count"`
		}
		// Both counts are read in one attempt and retried until they agree:
		// background extraction of the earlier turns can add a memory between
		// two separate reads.
		var ownerCount, viewerCount count
		err := eventually(t, "the viewer to count every memory", visibleWithin(t), func() (bool, error) {
			if err := owner.rest(get, "/memories/count?datapack_id="+dp.ID, nil, httpOK, &ownerCount); err != nil {
				return false, err
			}
			if err := viewer.rest(get, "/memories/count?datapack_id="+dp.ID, nil, httpOK, &viewerCount); err != nil {
				return false, err
			}
			return ownerCount.Count >= 1 && viewerCount.Count == ownerCount.Count, nil
		})
		h.Set("owner_count", ownerCount.Count)
		h.Set("viewer_count", viewerCount.Count)
		if err != nil {
			return fmt.Errorf("the viewer counts %d memories, want everyone's (%d): %w", viewerCount.Count, ownerCount.Count, err)
		}
		// user_id is ignored for a viewer: filtering by it would say whose they are.
		for _, q := range []string{"", "&user_id=" + owner.acct.ID} {
			r, err := viewer.api.Raw(t, get, "/memories?page_size=100&datapack_id="+dp.ID+q, nil)
			if err != nil {
				return err
			}
			if err := wantStatus("the viewer's /memories"+q, r, httpOK, ""); err != nil {
				return err
			}
			if !strings.Contains(string(r.Body), fact) {
				return fmt.Errorf("the viewer's /memories%s lacks the owner's memory: %s", q, truncateBody(r.Body))
			}
			if err := namesNoOwner("the viewer's /memories"+q, r.Body); err != nil {
				return err
			}
		}
		return refused(t, "the viewer deletes the owner's memory", viewer.api, del,
			"/memories/"+added.Results[0].ID+"?datapack_id="+dp.ID, nil, statusNotFound, "")
	}); err != nil {
		errs = append(errs, err)
	}

	docName := "notes-" + mk + ".txt"
	if err := t.Step("viewer_is_not_told_who_uploaded", func(h *core.StepHandle) error {
		if _, err := owner.api.UploadKBFile(t, dp.ID, docName, []byte("Release notes live in the wiki. "+mk+"\n")); err != nil {
			return err
		}
		type doc struct {
			Name       string  `json:"document_name"`
			Uploader   string  `json:"uploaded_by_email"`
			UploaderID *string `json:"uploaded_by_user_id"`
		}
		find := func(u *user) (*doc, error) {
			var page struct {
				Items []doc `json:"items"`
			}
			if err := u.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/documents?page_size=100", nil, httpOK, &page); err != nil {
				return nil, err
			}
			for i := range page.Items {
				if page.Items[i].Name == docName {
					return &page.Items[i], nil
				}
			}
			return nil, nil
		}
		var seen *doc
		err := eventually(t, docName+" in the contributor's document list, with its uploader", visibleWithin(t), func() (bool, error) {
			var err error
			seen, err = find(contributor)
			return seen != nil && seen.Uploader == owner.acct.Email, err
		})
		if err != nil {
			return fmt.Errorf("%w; contributor sees %+v", err, seen)
		}
		v, err := find(viewer)
		if err != nil {
			return err
		}
		if v == nil {
			return fmt.Errorf("the viewer's document list lacks %s, which the contributor sees", docName)
		}
		if v.Uploader != "" || v.UploaderID != nil {
			return fmt.Errorf("the viewer is told %s was uploaded by %q (%v), want no uploader", docName, v.Uploader, v.UploaderID)
		}
		type entry struct {
			Type     string  `json:"type"`
			Name     *string `json:"document_name"`
			Uploader *string `json:"uploaded_by_email"`
		}
		upload := func(u *user) (*entry, error) {
			var log struct {
				Items []entry `json:"items"`
			}
			if err := u.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/activity-log?limit=50", nil, httpOK, &log); err != nil {
				return nil, err
			}
			for i := range log.Items {
				if log.Items[i].Name != nil && *log.Items[i].Name == docName {
					return &log.Items[i], nil
				}
			}
			return nil, nil
		}
		var seenLog *entry
		if err := eventually(t, docName+" in the contributor's activity log, with its uploader", visibleWithin(t), func() (bool, error) {
			var err error
			seenLog, err = upload(contributor)
			return seenLog != nil && seenLog.Uploader != nil && *seenLog.Uploader == owner.acct.Email, err
		}); err != nil {
			return fmt.Errorf("%w; contributor's entry %+v", err, seenLog)
		}
		vLog, err := upload(viewer)
		if err != nil {
			return err
		}
		if vLog == nil {
			return fmt.Errorf("the viewer's activity log lacks the upload of %s, which the contributor's has", docName)
		}
		if vLog.Uploader != nil {
			return fmt.Errorf("the viewer's activity log names the uploader of %s: %q", docName, *vLog.Uploader)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("viewer_cannot_upload_an_artifact", func(*core.StepHandle) error {
		return refused(t, "the viewer's artifact upload", viewer.api, post, "/datapacks/"+dp.ID+"/artifacts",
			map[string]string{"filename": "viewer.txt", "content_base64": "aGVsbG8=", "content_type": "text/plain"},
			statusForbidden, "FORBIDDEN")
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("viewer_cannot_keep_as_memory", func(*core.StepHandle) error {
		return refused(t, "the viewer's keep-as-memory", viewer.api, post, "/keep-as-memory",
			map[string]any{"datapack_id": dp.ID, "memory_ids": []string{"00000000-0000-0000-0000-000000000000"}}, statusForbidden, "FORBIDDEN")
	}); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
