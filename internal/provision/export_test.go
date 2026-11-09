package provision

// SetBeforePolicyUpdate makes SetPolicy call hook between reading the policy
// and updating it.
func SetBeforePolicyUpdate(p *Provisioner, hook func()) { p.beforePolicyUpdate = hook }
