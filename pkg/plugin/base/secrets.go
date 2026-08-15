package base

// SecretMaterializer resolves generation-owned credentials after schema
// decoding and before PostInit.
type SecretMaterializer interface {
	MaterializeSecrets() error
}

// MaterializePluginSecrets runs the optional pre-PostInit secret phase.
func MaterializePluginSecrets(p any) error {
	materializer, ok := p.(SecretMaterializer)
	if !ok {
		return nil
	}
	return materializer.MaterializeSecrets()
}
