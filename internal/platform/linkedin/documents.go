// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// LinkedIn document ads (#665).
//
// A document create (CampaignInput.Format FormatDocument) is a SPONSORED_UPDATES campaign whose
// one creative references one dark post carrying a PDF uploaded through the Documents API. Sources,
// li-lms-2026-11, consulted 2026-11-25 (the live paused probe owed in #665 confirms the shape):
//
//   - "Documents API"
//     (https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/documents-api?view=li-lms-2026-11):
//     POST /rest/documents?action=initializeUpload with {"initializeUploadRequest":{"owner":
//     <organization URN>}} -> {"value":{"uploadUrl","document","uploadUrlExpiresAt"}}; then the
//     file is uploaded to uploadUrl; GET /rest/documents/{urn} reports status PROCESSING,
//     PROCESSING_FAILED, AVAILABLE or WAITING_UPLOAD ("SYNCHRONOUS_UPLOAD is not supported"). "The
//     file size can't exceed 100MB and 300 pages"; PPT, PPTX, DOC, DOCX and PDF. The post's content
//     is {"media": {"title": ..., "id": "urn:li:document:..."}}. For an organization owner "the
//     caller must have ADMIN or DSC permissions for the company page" — the dark post's own
//     requirement, so the upload adds none.
//   - "Document Ads API"
//     (https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads/advertising-targeting/version/document-ads?view=li-lms-2026-11):
//     the post's content is {"media": {"title", "id": <DocumentUrn>}}, and "For Document Ads, the
//     content field requires a documentAd object" — the creative's content is {"documentAd":
//     {"reference": <post>}}, with an optional gatedLeadgenPreviewPageCount for lead gen content.
//   - "Campaign Objectives": the document format (SPONSORED_UPDATE_NATIVE_DOCUMENT) is listed under
//     Brand Awareness and Engagement (optimisation goal Reach: MAX_REACH, CPM) and Lead Generation
//     (MAX_LEAD, CPM), and NOT under Website Visits or Website Conversions. "Create and Manage
//     Campaigns" lists no document campaign format and "If no Ad format is set, it's set by the
//     first creative created under that campaign", so the campaign is created without `format`.
//   - LinkedIn's document ads guide: headline 70 characters; the Sponsored Content intro text
//     ceiling this service already applies (600 characters).
//
// Decision 2026-11-25 (Misha): a document create is ENGAGEMENT by default (MAX_REACH, automatic
// only, name segment "Engagement"); objective LEAD_GENERATION with a Lead Gen Form makes it a gated
// document (MAX_LEAD, the form on the creative, e.g. a sponsor prospectus); Website Visits and
// Website Conversions are refused. The ENGAGEMENT objective is created only for a document.
//
// IDEMPOTENCY is the image path's, by the same construction: the upload runs AFTER the campaign is
// ensured and BEFORE the dark post, so every exit from the first initializeUpload on returns a
// non-nil partial result (claim retained, never re-dispatched), and the minted URN is recorded on
// the result (DocumentURNs). A document has no landing page: the PDF is the content, so no UTM
// parameters are sent; a lead gen document's form is the creative's leadgenCallToAction.

// FormatDocument is a document ad campaign whose one creative carries CampaignInput.Document.
const FormatDocument = "document"

// campaignFormatDocument is the campaign format LinkedIn reports for a document ad campaign. It is
// never SENT (see the file comment); findCampaignByNameInGroup uses it to tell a document campaign
// from an image, video or carousel one.
const campaignFormatDocument = "SPONSORED_UPDATE_NATIVE_DOCUMENT"

// campaignFormatUnsupported is the format LinkedIn reports for a document campaign before its
// creative is attached (#690). findCampaignByNameInGroup treats it as an unreported format.
const campaignFormatUnsupported = "UNSUPPORTED"

// ObjectiveEngagement and OptimizationTargetMaxReach are a document create's default objective and
// its automated target (decision 2026-11-25).
const (
	ObjectiveEngagement        = "ENGAGEMENT"
	OptimizationTargetMaxReach = "MAX_REACH"
)

// Document limits (sources above). Refused, never truncated.
const (
	DocumentTitleMaxChars     = 70
	DocumentIntroTextMaxChars = 600
	// MaxDocumentBytes reads "100MB" decimally. The creative-asset store caps a file at 30 MiB,
	// so in practice that is the ceiling a dispatch meets first.
	MaxDocumentBytes = 100_000_000
)

// Defaults for the document availability poll: a document is converted to pages, so it is
// given a longer shared wait than an image, still inside the two-minute provider call.
const (
	defaultDocumentPollInterval     = 2 * time.Second
	defaultDocumentAvailabilityWait = 60 * time.Second
)

// documentURNRE matches a Documents API URN.
var documentURNRE = regexp.MustCompile(`^urn:li:document:[A-Za-z0-9_-]+$`)

// documentMedia is what awaitMediaAvailable polls for a document.
var documentMedia = mediaKind{noun: "document", resource: "documents"}

// ErrDocumentInvalid marks a stored document LinkedIn cannot accept, decided locally.
var ErrDocumentInvalid = errors.New("linkedin: document not accepted")

// DocumentUpload is the stored PDF to upload: its bytes and the verified MIME type.
type DocumentUpload struct {
	Data     []byte
	MimeType string
}

// WithDocumentPolling overrides the document availability poll interval and wait. Non-positive
// values keep the defaults.
func WithDocumentPolling(interval, wait time.Duration) Option {
	return func(c *Client) {
		if interval > 0 {
			c.documentPollInterval = interval
		}
		if wait > 0 {
			c.documentAvailabilityWait = wait
		}
	}
}

// ValidateDocument reports whether in can be uploaded as a document ad: a PDF (the only document
// type the asset store holds) with the PDF header, at most MaxDocumentBytes. The page count is
// LinkedIn's to judge (a document over 300 pages ends PROCESSING_FAILED before any post).
func ValidateDocument(in DocumentUpload) error {
	if len(in.Data) == 0 {
		return fmt.Errorf("%w: the document has no bytes", ErrDocumentInvalid)
	}
	if in.MimeType != "application/pdf" {
		return fmt.Errorf("%w: only PDF documents are supported", ErrDocumentInvalid)
	}
	if len(in.Data) > MaxDocumentBytes {
		return fmt.Errorf("%w: the document is %d bytes; LinkedIn document ads accept at most %d", ErrDocumentInvalid, len(in.Data), MaxDocumentBytes)
	}
	if !bytes.HasPrefix(in.Data, []byte("%PDF-")) {
		return fmt.Errorf("%w: the document bytes are not a PDF file", ErrDocumentInvalid)
	}
	return nil
}

// ValidateDocumentFields checks a document create's text before anything is created: the title
// (the post's media title) and the intro text (its commentary), after the normalisation the wire
// copy gets, and that a stored document is named.
func ValidateDocumentFields(assetID, title, introText string) error {
	if strings.TrimSpace(assetID) == "" {
		return fmt.Errorf("%w: a document create requires documentAssetId", ErrFormatInvalid)
	}
	head := normalizeCreativeText(title)
	if head == "" {
		return fmt.Errorf("%w: documentTitle is required and must not be empty, whitespace-only, or dash-only after normalization", ErrFormatInvalid)
	}
	if c := len([]rune(head)); c > DocumentTitleMaxChars {
		return fmt.Errorf("%w: documentTitle is %d characters; LinkedIn allows at most %d", ErrFormatInvalid, c, DocumentTitleMaxChars)
	}
	intro := normalizeCreativeText(introText)
	if intro == "" {
		return fmt.Errorf("%w: documentIntroText is required and must not be empty, whitespace-only, or dash-only after normalization", ErrFormatInvalid)
	}
	if c := len([]rune(intro)); c > DocumentIntroTextMaxChars {
		return fmt.Errorf("%w: documentIntroText is %d characters; LinkedIn allows at most %d", ErrFormatInvalid, c, DocumentIntroTextMaxChars)
	}
	return nil
}

// documentObjective is a document create's objective: ENGAGEMENT when none is asked, LEAD_GENERATION
// as asked, and a refusal for anything else (decision 2026-11-25).
func documentObjective(objective string) (string, error) {
	switch objective {
	case "", ObjectiveEngagement:
		return ObjectiveEngagement, nil
	case ObjectiveLeadGeneration:
		return ObjectiveLeadGeneration, nil
	}
	return "", fmt.Errorf("%w: a document create is %s (the default) or %s; LinkedIn offers document ads no %s", ErrFormatInvalid, ObjectiveEngagement, ObjectiveLeadGeneration, objective)
}

// documentInitResponse is the Documents API initializeUpload response body.
type documentInitResponse struct {
	Value struct {
		UploadURL string `json:"uploadUrl"`
		Document  string `json:"document"`
	} `json:"value"`
}

// uploadDocument runs initializeUpload, the PUT and the availability poll for the create's
// document, returning the minted URN (empty when none is known) and the Steps. No call is retried.
func (c *Client) uploadDocument(ctx context.Context, accountID string, doc DocumentUpload) (string, []string, error) {
	fail := func(urn string, err error) (string, []string, error) {
		var ie *imageUploadError
		if errors.As(err, &ie) && ie.unconfirmed {
			return urn, []string{fmt.Sprintf("Document UNCONFIRMED (an unreferenced document may exist in LinkedIn; it serves nothing): %s", err.Error())}, err
		}
		return urn, []string{fmt.Sprintf("Document NOT uploaded: %s", err.Error())}, err
	}
	if err := ValidateDocument(doc); err != nil {
		return fail("", &imageUploadError{err: err})
	}
	owner, err := c.orgURN(accountID)
	if err != nil {
		return fail("", &imageUploadError{err: fmt.Errorf("document upload: %w", err)})
	}
	const initPath = "documents?action=initializeUpload"
	body, err := json.Marshal(map[string]any{"initializeUploadRequest": map[string]any{"owner": owner}})
	if err != nil {
		return fail("", &imageUploadError{err: fmt.Errorf("marshal initializeUpload: %w", err)})
	}
	raw, _, err := c.mediaRequest(ctx, http.MethodPost, c.baseURL+"/"+initPath, initPath, body, "application/json", true)
	if err != nil {
		return fail("", &imageUploadError{err: fmt.Errorf("document upload: initializeUpload: %w", err), unconfirmed: createOutcomeAmbiguous(err)})
	}
	var resp documentInitResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil || !documentURNRE.MatchString(resp.Value.Document) {
		return fail("", &imageUploadError{err: errors.New("document upload: initializeUpload returned no usable document URN"), unconfirmed: true})
	}
	urn := resp.Value.Document
	if !c.trustedUploadURL(resp.Value.UploadURL) {
		return fail(urn, &imageUploadError{err: fmt.Errorf("document upload: initializeUpload returned an upload URL outside LinkedIn for %s; nothing was uploaded", urn)})
	}
	if _, _, err := c.mediaRequest(ctx, http.MethodPut, resp.Value.UploadURL, "document upload URL", doc.Data, doc.MimeType, false); err != nil {
		return fail(urn, &imageUploadError{err: fmt.Errorf("document upload: upload of %s failed: %w", urn, err), unconfirmed: createOutcomeAmbiguous(err)})
	}
	steps := []string{fmt.Sprintf("Document uploaded: %s", urn)}
	const key = "document"
	if err := c.awaitMediaAvailable(ctx, documentMedia, c.documentPollInterval, c.documentAvailabilityWait, []string{key}, map[string]string{key: urn}); err != nil {
		return urn, append(steps, fmt.Sprintf("Document NOT used (no dark post was created): %s", err.Error())), err
	}
	return urn, append(steps, "Document AVAILABLE"), nil
}

// createDocumentDarkPost creates a dark post (feedDistribution NONE) whose content is the
// document and returns its post URN. The envelope is createDarkPost's; the content is the
// Documents API's {"media": {"title", "id"}}. There is no landing page.
func (c *Client) createDocumentDarkPost(ctx context.Context, accountID, introText, title, documentURN string) (string, error) {
	author, err := c.orgURN(accountID)
	if err != nil {
		return "", err
	}
	body := darkPostEnvelope(author, normalizeCreativeText(introText), accountID)
	body["content"] = map[string]any{"media": map[string]any{"title": normalizeCreativeText(title), "id": documentURN}}
	return c.postDarkPost(ctx, body)
}

// createDocumentCreative creates the DRAFT creative of a document ad. Unlike an article, image or
// video creative, whose content is {"reference": <post>}, a document creative's content is
// {"documentAd": {"reference": <post>}} ("Document Ads API", li-lms-2026-11: "For Document Ads, the
// content field requires a documentAd object"). A gated document carries the Lead Gen Form as
// leadgenCallToAction like any lead generation creative; gatedLeadgenPreviewPageCount is optional
// and not sent, so LinkedIn's default preview applies.
func (c *Client) createDocumentCreative(ctx context.Context, accountID, campaignID, shareURN, adName string, leadGen *LeadGen) (string, error) {
	return c.postCreative(ctx, accountID, campaignID, map[string]any{"documentAd": map[string]any{"reference": shareURN}}, adName, leadGen)
}

// createDocumentAd uploads the document, then creates the dark post and the one DRAFT creative
// (carrying the Lead Gen Form on a lead generation create). It returns how many creatives were
// created (0 or 1), the minted document URN, the Steps and the first failure. NOT idempotent, as
// every creative path: an ambiguous post or creative failure is reported UNCONFIRMED.
func (c *Client) createDocumentAd(ctx context.Context, accountID, campaignID, eventName string, in CampaignInput) (int, string, []string, error) {
	urn, steps, err := c.uploadDocument(ctx, accountID, *in.Document)
	if err != nil {
		return 0, urn, steps, fmt.Errorf("document failed after the campaign was ensured (no dark post or creative was created): %w", err)
	}
	shareURN, err := c.createDocumentDarkPost(ctx, accountID, in.DocumentIntroText, in.DocumentTitle, urn)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			steps = append(steps, "Document dark post outcome UNCONFIRMED (timeout, server error, or an in-flight cancellation); it may have been created — verify before recreating (a dark post has no idempotency lookup)")
			return 0, urn, steps, fmt.Errorf("document dark post UNCONFIRMED: %w; the dark post MAY have been created and has no idempotency lookup, so do NOT blindly retry; inspect the returned partial result", err)
		}
		return 0, urn, steps, fmt.Errorf("document dark post failed: %w; inspect the returned partial result", err)
	}
	steps = append(steps, fmt.Sprintf("Document dark post: %s", shareURN))
	creativeID, err := c.createDocumentCreative(ctx, accountID, campaignID, shareURN, fmt.Sprintf("%s | document-1", eventName), in.LeadGen)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			steps = append(steps, fmt.Sprintf("Document creative outcome UNCONFIRMED (timeout, server error, or an in-flight cancellation); it may have been created — verify before recreating (orphaned dark post: %s)", shareURN))
			return 0, urn, steps, fmt.Errorf("document creative UNCONFIRMED: %w; the dark post %q was already created and the creative MAY have been, and neither has an idempotency lookup, so do NOT blindly retry; inspect the returned partial result", err, shareURN)
		}
		return 0, urn, steps, fmt.Errorf("document creative failed: %w; the dark post %q was already created and has no idempotency lookup, so do NOT blindly retry; inspect the returned partial result", err, shareURN)
	}
	steps = append(steps, fmt.Sprintf("Document creative (DRAFT): %s", creativeID))
	return 1, urn, steps, nil
}
