# 2026-11-17 — #564 video.go comment names the website card again

**Docs** — The doc comment on the X video upload in `internal/platform/twitter/video.go` again says the media key goes on every authored tweet, or into the website card's MEDIA component when a card is requested (#564 gap 6). A merge while the X gaps were retargeted to main had restored the older wording from before part 2b ("on the authored tweet"). The code and the tests (`TestTwitterVideo_IsCardMedia`, `TestCreateCampaign_VideoIsTheCardMedia`) were not affected.
