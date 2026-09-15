// SPDX-License-Identifier: MPL-2.0

package process

import "testing"

// Inheriting an actor is not impersonating one.
//
// The regression this pins: with_options(...) and with_context(...) both copy
// the caller's own actor out of context and set hasActor, and with_context
// then read that flag as "the caller asked for someone else's". So the pair
// demanded the right to impersonate from a caller that had impersonated
// nobody — while each call on its own was fine, which is why it survived
// every test until a compositor used both to pass its own name to a window.
func TestInheritedActorIsNotACustomOne(t *testing.T) {
	inherited := &Spawner{hasActor: true, hasScope: true}
	if inherited.customActor || inherited.customScope {
		t.Fatal("carrying an actor must not read as asking for another one")
	}

	clone := cloneSpawner(inherited)
	if clone.customActor || clone.customScope {
		t.Fatal("cloning must not invent a custom actor")
	}
	if !clone.hasActor || !clone.hasScope {
		t.Fatal("cloning must keep inheritance, or the child loses its actor")
	}

	// And the privilege flag has to survive a clone, or with_actor followed
	// by with_context would quietly stop being checked.
	impersonating := cloneSpawner(&Spawner{hasActor: true, customActor: true})
	if !impersonating.customActor {
		t.Fatal("a custom actor must survive cloning, or the check disappears")
	}
}
