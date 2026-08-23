//go:build !darwin

package usage

func descriptorHasExtendedACL(int) (bool, error) {
	return false, nil
}
