package main

// camera_observer_faces.go — the observer's per-cycle FACE RECOGNITION step.
//
// The activity logger used to name enrolled people only by DESCRIPTION (the
// KNOWN PEOPLE roster in the system prompt asks the model to recognize them by
// appearance). That reliably missed employees who look ordinary on camera. This
// step gives the logger actual biometric recognition: on each cycle it runs the
// localhost face sidecar (camera_face.go) over the frames already sampled for
// the window, matches every detected face against the site's enrolled human
// avatars' reference embeddings (the same cosine-similarity path avatar_check
// uses), and feeds the confirmed hits ("Heba — Reception at 17:42, similarity
// 0.58") into the cycle's user prompt as a CONFIRMED IDENTITIES block so the
// model names them reliably instead of guessing.
//
// It is BOUNDED to keep cost sane:
//   - runs only when PROXY_CAMERA_OBSERVER_FACE_REC AND the stream's face_rec
//     flag are on, the sidecar is enabled, and at least one enrolled human
//     avatar carries a usable face embedding;
//   - detects on at most PROXY_CAMERA_OBSERVER_FACE_MAX_FRAMES frames per
//     camera (evenly spread across the window) and a hard per-cycle cap
//     (camObserverFaceMaxDetect);
//   - detects on MAIN-quality archive frames (faces need pixels; the SUB frames
//     the sheets use embed too poorly to match) with a fallback to the sampled
//     SUB bytes when no main frame exists there — archive-only, never a DVR pull;
//   - each detected face is assigned to its single best-matching avatar (argmax
//     cosine ≥ threshold), so one person never lights up two names;
//   - a sidecar that fails its first calls (with nothing yet matched) aborts the
//     step, and the cycle falls back to description-only naming exactly as
//     before — face rec is additive, never a new failure mode.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// camObserverFaceDetectFn is the sidecar seam (mirrors camObserverAnalyzeFn /
// camObserverFetchFrameFn) so tests can script detections without a live
// face-embedding sidecar.
var camObserverFaceDetectFn = camFaceDetect

// camObserverFaceMaxDetect is the hard ceiling on sidecar detections per cycle,
// independent of camera count — a runaway roster can never turn one journal
// cycle into hundreds of embed calls.
const camObserverFaceMaxDetect = 40

// camObserverIdentity is one enrolled person the face-recognition engine
// confirmed in a cycle's sampled frames: the best (highest-similarity) sighting
// plus the display names of any OTHER cameras they also matched on.
type camObserverIdentity struct {
	Name          string
	AvatarID      string    // enrolled avatar id (for the presence/attendance layer)
	Kind          string
	CameraID      string    // camera of the best sighting
	CameraName    string    // display name of that camera
	T             time.Time // instant of the best sighting
	Similarity    float64   // best cosine similarity across the window
	AlsoOn        []string  // display names of other cameras the person matched on
	FirstT        time.Time // EARLIEST sighting instant this cycle (arrival within the cycle)
	FirstCameraID string    // camera of the earliest sighting
}

// camFaceCandidate is one enrolled human avatar the recognizer can match a
// detected face against, with its usable reference embeddings resolved once.
type camFaceCandidate struct {
	av     camAvatar
	embeds [][]float32
}

// camObserverFaceThreshold resolves the cosine floor for a confirmed identity:
// the observer-specific override (PROXY_CAMERA_OBSERVER_FACE_THRESHOLD) when
// set, else the general FaceMatchThreshold (0.40) — so an operator can tighten
// the journal's naming without touching interactive avatar_check.
func camObserverFaceThreshold(cfg config) float64 {
	if cfg.CameraObserverFaceThreshold > 0 {
		return cfg.CameraObserverFaceThreshold
	}
	return avToolFaceThreshold(cfg)
}

// camObserverFaceMaxFramesFor resolves the per-camera detection cap (default 4).
func camObserverFaceMaxFramesFor(cfg config) int {
	if n := cfg.CameraObserverFaceMaxFrames; n > 0 {
		return n
	}
	return 4
}

// camObserverFaceCandidates builds the match set: enabled HUMAN avatars (in the
// scope the caller already applied) that carry at least one usable face
// embedding — a per-reference embedding or the cached centroid. Description-only
// avatars are skipped: there is nothing to match a detected face against.
func camObserverFaceCandidates(db *sql.DB, avatars []camAvatar, cfg config) []camFaceCandidate {
	if !camFaceEnabled(cfg) {
		return nil
	}
	var out []camFaceCandidate
	for _, a := range avatars {
		if !a.Enabled || avToolType(a) != "human" {
			continue
		}
		var embeds [][]float32
		if media, err := listAvatarMedia(db, a.ID); err == nil {
			embeds = avToolRefEmbeddings(a, media)
		} else if v := camEmbeddingFromBlob(a.Embedding); len(v) > 0 {
			embeds = [][]float32{v}
		}
		if len(embeds) == 0 {
			continue
		}
		out = append(out, camFaceCandidate{av: a, embeds: embeds})
	}
	return out
}

// camObserverThinFrames evenly downsamples a camera's sampled frames to at most
// max, sorted by time and preserving both endpoints (max==1 keeps the latest —
// the frame closest to the window's end). The camObserverThinTimes shape, for
// frames.
func camObserverThinFrames(fs []camObserverFrame, max int) []camObserverFrame {
	if max <= 0 || len(fs) <= max {
		return fs
	}
	sorted := append([]camObserverFrame(nil), fs...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].T.Before(sorted[b].T) })
	if max == 1 {
		return sorted[len(sorted)-1:]
	}
	out := make([]camObserverFrame, 0, max)
	last := len(sorted) - 1
	prev := -1
	for i := 0; i < max; i++ {
		idx := i * last / (max - 1)
		if idx <= prev {
			idx = prev + 1
		}
		if idx > last {
			idx = last
		}
		out = append(out, sorted[idx])
		prev = idx
	}
	return out
}

// camObserverFaceImage returns the best available bytes to detect faces on for
// a sampled frame: a MAIN-quality archive frame (faces need pixels — the SUB
// frames the composite sheets use are too low-res to embed reliably) at the
// frame's second, falling back to the already-sampled SUB bytes on disk when
// the archive has no main frame there (main is keyframe-only) or S3 is off. The
// bool reports whether the returned bytes are main-quality (diagnostics only).
// Mirrors faceScan/avatar_find: MAIN is keyed on the 1-fps grid at the exact
// second, not the 30s snapshot snap.
func camObserverFaceImage(ctx context.Context, cfg config, f camObserverFrame) ([]byte, bool) {
	if camS3Enabled(cfg) {
		if b, err := s3GetObject(ctx, cfg, camS3FrameKey(f.Cam.ID, "main", f.T.Truncate(time.Second))); err == nil && len(b) > 0 {
			return b, true
		}
	}
	b, _ := os.ReadFile(f.Path)
	return b, false
}

// camObserverRecognizeFaces runs the sidecar over the cycle's sampled frames and
// returns the enrolled people confidently recognized, best sighting per person.
// Returns nil when face rec has nothing to do (no candidate enrolled), the
// sidecar is unreachable, or nothing matched — the caller then simply omits the
// CONFIRMED IDENTITIES block and the model falls back to description-only naming.
func camObserverRecognizeFaces(ctx context.Context, cfg config, db *sql.DB, cams []camera,
	frames map[string][]camObserverFrame, avatars []camAvatar, loc *time.Location) []camObserverIdentity {

	candidates := camObserverFaceCandidates(db, avatars, cfg)
	if len(candidates) == 0 {
		// Log even the no-op: "recognition did nothing" must be distinguishable
		// from "sidecar found nobody" when diagnosing a missed employee.
		camlog("info", "observer_faces", map[string]any{"ok": true, "candidates": 0, "reason": "no enrolled human faces in scope"})
		return nil
	}
	threshold := camObserverFaceThreshold(cfg)
	maxPerCam := camObserverFaceMaxFramesFor(cfg)
	budget := maxPerCam * len(cams)
	if budget > camObserverFaceMaxDetect {
		budget = camObserverFaceMaxDetect
	}

	camNameByID := make(map[string]string, len(cams))
	for _, c := range cams {
		camNameByID[c.ID] = camDisplayName(c)
	}

	// Best sighting per avatar id, plus the set of cameras it matched on and the
	// earliest sighting instant/camera (the arrival within this cycle).
	type bestHit struct {
		idty     camObserverIdentity
		cams     map[string]bool
		firstT   time.Time
		firstCam string
	}
	bests := map[string]*bestHit{}

	detects, faceErrs, matched, facesSeen, mainHits := 0, 0, 0, 0, 0
	bestSimSeen := 0.0 // highest cosine of ANY detected face vs ANY candidate (even below threshold) — the key "close but rejected?" diagnostic
	for _, c := range cams {
		fs := frames[c.ID]
		if len(fs) == 0 {
			continue
		}
		for _, f := range camObserverThinFrames(fs, maxPerCam) {
			if detects >= budget {
				break
			}
			img, isMain := camObserverFaceImage(ctx, cfg, f)
			if len(img) == 0 {
				continue
			}
			if isMain {
				mainHits++
			}
			detects++
			faces, ferr := camObserverFaceDetectFn(ctx, cfg, img)
			if ferr != nil {
				faceErrs++
				// Sidecar-down guard: if the first calls all fail and nothing has
				// matched, abort and let the cycle name people by description only.
				if faceErrs >= 3 && matched == 0 {
					camlog("warn", "observer_faces", map[string]any{"ok": false, "error": ferr.Error(), "detects": detects})
					return nil
				}
				continue
			}
			facesSeen += len(faces)
			// Each detected face is assigned to its single best-matching avatar
			// (argmax cosine), so two enrolled look-alikes never both claim one face.
			for _, face := range faces {
				bestSim := -1.0
				bestIdx := -1
				for i := range candidates {
					for _, ref := range candidates[i].embeds {
						if s := camCosine(face.Embedding, ref); s > bestSim {
							bestSim, bestIdx = s, i
						}
					}
				}
				if bestSim > bestSimSeen {
					bestSimSeen = bestSim
				}
				if bestIdx < 0 || bestSim < threshold {
					continue
				}
				matched++
				cand := candidates[bestIdx]
				b := bests[cand.av.ID]
				if b == nil {
					b = &bestHit{cams: map[string]bool{}}
					bests[cand.av.ID] = b
				}
				b.cams[c.ID] = true
				if b.firstT.IsZero() || f.T.Before(b.firstT) {
					b.firstT, b.firstCam = f.T, c.ID
				}
				if bestSim > b.idty.Similarity {
					b.idty = camObserverIdentity{
						Name: cand.av.Name, AvatarID: cand.av.ID, Kind: avToolType(cand.av),
						CameraID: c.ID, CameraName: camNameByID[c.ID],
						T: f.T, Similarity: bestSim,
					}
				}
			}
		}
		if detects >= budget {
			break
		}
	}

	if len(bests) == 0 {
		// Faces may have been detected but none cleared the threshold — log the
		// tally so a missed employee shows up as "faces=N matches=0" (tune the
		// threshold / add reference photos) vs "faces=0" (frames too low-res).
		camlog("info", "observer_faces", map[string]any{
			"ok": true, "candidates": len(candidates), "detects": detects, "main_hits": mainHits,
			"faces": facesSeen, "matches": 0, "identities": 0, "best_sim": fmt.Sprintf("%.2f", bestSimSeen), "threshold": threshold,
		})
		return nil
	}
	out := make([]camObserverIdentity, 0, len(bests))
	for _, b := range bests {
		for cid := range b.cams {
			if cid != b.idty.CameraID {
				b.idty.AlsoOn = append(b.idty.AlsoOn, camNameByID[cid])
			}
		}
		sort.Strings(b.idty.AlsoOn)
		b.idty.FirstT, b.idty.FirstCameraID = b.firstT, b.firstCam
		out = append(out, b.idty)
	}
	// Strongest match first — a stable, useful order for the prompt block.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].Name < out[j].Name
	})
	camlog("info", "observer_faces", map[string]any{
		"ok": true, "candidates": len(candidates), "detects": detects, "main_hits": mainHits,
		"faces": facesSeen, "matches": matched, "identities": len(out), "best_sim": fmt.Sprintf("%.2f", bestSimSeen), "threshold": threshold,
	})
	return out
}

// camObserverIdentityBlock renders the confirmed identities as a user-prompt
// block telling the model to name these specific people. It is deliberately
// SEPARATE from the system prompt's static KNOWN PEOPLE roster: that lists who
// COULD be recognized; this lists who the face engine actually matched in THIS
// window's frames. Returns "" when nothing was confirmed.
func camObserverIdentityBlock(identities []camObserverIdentity, loc *time.Location, tzName string) string {
	if len(identities) == 0 {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CONFIRMED IDENTITIES (face-recognition engine, THIS window, %s) — these enrolled people were matched against their reference photos, independent of the sheets above:\n", tzName)
	for _, id := range identities {
		fmt.Fprintf(&b, "- %s (%s) — best match on %s at %s (similarity %.2f)",
			id.Name, id.Kind, id.CameraName, id.T.In(loc).Format("15:04:05"), id.Similarity)
		if len(id.AlsoOn) > 0 {
			fmt.Fprintf(&b, "; also seen on %s", strings.Join(id.AlsoOn, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("Refer to each of these people BY NAME wherever they plausibly appear in the frames. ")
	b.WriteString("Anyone NOT in this list is unidentified — describe them by appearance and do NOT assign them an enrolled identity.")
	return b.String()
}
