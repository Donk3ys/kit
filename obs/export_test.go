package obs

// Sampler exposes the unexported sampler resolver to the external test
// package, so the "zero ratio means record everything" contract can be pinned.
var Sampler = sampler
