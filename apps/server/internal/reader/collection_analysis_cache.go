package reader

import (
	"encoding/json"
	"errors"
)

func analysisWireResult(result AnalysisResult) *CollectionAnalysisResult {
	wire := &CollectionAnalysisResult{SchemaVersion: result.SchemaVersion, TitleZH: result.TitleZH, AbstractZH: result.AbstractZH, Assessments: []CollectionAnalysisTask{}}
	for _, assessment := range result.Assessments {
		a := CollectionAnalysisTask{TaskID: assessment.TaskID, ProfileHash: assessment.ProfileHash, ReasonZH: assessment.ReasonZH, RelevanceScore: assessment.RelevanceScore, Evidence: []CollectionEvidence{}}
		for _, e := range assessment.Evidence {
			a.Evidence = append(a.Evidence, CollectionEvidence{Field: e.Field, Quote: e.Quote})
		}
		wire.Assessments = append(wire.Assessments, a)
	}
	return wire
}

// Translation is reusable for the same paper/model. Each judgment additionally
// requires its current task/profile; changing the requested task set does not
// discard independent valid judgments or make old ones effective.
func (s *Store) cachedAnalysisFragments(candidate CollectionCandidate, materials []AnalysisTaskMaterial, fingerprint string) ([]CollectionAnalysis, []AnalysisTaskMaterial, error) {
	out := []CollectionAnalysis{}
	if fingerprint == "" {
		return out, materials, nil
	}
	wanted := map[string]AnalysisTaskMaterial{}
	for _, material := range materials {
		wanted[material.TaskID] = material
	}
	seen := map[string]bool{}
	translated := false
	rows, err := s.DB.Query("SELECT cache_key,arxiv_id,content_hash,profile_hash,model_fingerprint,prompt_version,result_version,title_zh,abstract_zh,assessments_json,attempt,created_at,updated_at FROM collection_analyses WHERE arxiv_id=? AND content_hash=? AND model_fingerprint=? AND prompt_version=? AND result_version=? AND status='succeeded' ORDER BY updated_at DESC,cache_key", candidate.ArXivID, candidate.ContentHash, fingerprint, analysisPromptVersion, collectionResultVersion)
	if err != nil {
		return out, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a CollectionAnalysis
		var title, abstract, body string
		if err := rows.Scan(&a.CacheKey, &a.ArXivID, &a.ContentHash, &a.ProfileHash, &a.ModelFingerprint, &a.PromptVersion, &a.ResultVersion, &title, &abstract, &body, &a.Attempt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return out, nil, err
		}
		var raw AnalysisResult
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			return out, nil, errInvalidAnalysis
		}
		input := AnalysisInput{Title: candidate.Title, Abstract: candidate.Abstract, Categories: candidate.Categories}
		for _, assessment := range raw.Assessments {
			input.Tasks = append(input.Tasks, AnalysisTaskMaterial{TaskID: assessment.TaskID, ProfileHash: assessment.ProfileHash, WantsScore: true})
		}
		validated, err := DecodeAnalysisResult(body, input)
		if err != nil {
			return out, nil, err
		}
		if validated.TitleZH != title || validated.AbstractZH != abstract {
			return out, nil, errors.New("已缓存译文与结果不一致")
		}
		a.Result = analysisWireResult(validated)
		filtered := []CollectionAnalysisTask{}
		for _, assessment := range a.Result.Assessments {
			material, ok := wanted[assessment.TaskID]
			if ok && material.WantsScore && assessment.ProfileHash == material.ProfileHash && !seen[material.TaskID] {
				filtered = append(filtered, assessment)
				seen[material.TaskID] = true
			}
		}
		a.Result.Assessments = filtered
		a.Status = analysisSucceeded
		if !translated || len(filtered) > 0 {
			out = append(out, a)
		}
		translated = true
	}
	if err := rows.Err(); err != nil {
		return out, nil, err
	}
	missing := []AnalysisTaskMaterial{}
	for _, material := range materials {
		if !translated || material.WantsScore && !seen[material.TaskID] {
			missing = append(missing, material)
		}
	}
	return out, missing, nil
}
