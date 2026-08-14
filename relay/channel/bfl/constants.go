package bfl

const (
	// ChannelName identifies the Black Forest Labs (BFL) channel.
	ChannelName = "bfl"

	ModelFluxPro11      = "flux-pro-1.1"
	ModelFluxPro11Ultra = "flux-pro-1.1-ultra"
	ModelFluxPro        = "flux-pro"
	ModelFluxDev        = "flux-dev"
	ModelFluxKontextPro = "flux-kontext-pro"
	ModelFluxKontextMax = "flux-kontext-max"
)

var ModelList = []string{
	ModelFluxPro11,
	ModelFluxPro11Ultra,
	ModelFluxPro,
	ModelFluxDev,
	ModelFluxKontextPro,
	ModelFluxKontextMax,
}

// aspectRatioModels describes BFL models whose framing is controlled via an
// "aspect_ratio" string instead of explicit "width"/"height" pixel fields.
var aspectRatioModels = map[string]bool{
	ModelFluxPro11Ultra: true,
	ModelFluxKontextPro: true,
	ModelFluxKontextMax: true,
}
