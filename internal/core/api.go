package core

import "github.com/Schallophy/literouter/internal/model"

var pathToAPI = map[string]model.ApiKind{
	"chat/completions": model.ApiChat,
	"completions":      model.ApiCompletions,
	"responses":        model.ApiResponses,
	"embeddings":       model.ApiEmbeddings,
}

// APIKindForPath maps the OpenAI path suffix to its protocol.
func APIKindForPath(path string) model.ApiKind {
	if kind, ok := pathToAPI[path]; ok {
		return kind
	}
	return model.ApiChat
}

// ApisFor returns the declared APIs of a model, defaulting to chat.
func ApisFor(apis map[string][]model.ApiKind, code string) []model.ApiKind {
	if list, ok := apis[code]; ok && len(list) > 0 {
		return list
	}
	return model.DefaultModelApis
}

// SupportsAPI reports whether the model declares the given protocol.
func SupportsAPI(apis map[string][]model.ApiKind, code string, kind model.ApiKind) bool {
	for _, entry := range ApisFor(apis, code) {
		if entry == kind {
			return true
		}
	}
	return false
}
