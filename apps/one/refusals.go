package main

// SPEC 22: a refusal reads `Refused by guard: <guard name>` and one plain
// sentence saying why, so the agent changes its advice and the owner can read the
// reason in the record. One sentence per guard and case; none names a secret,
// a sender or the content of a message.
const (
	whyReasonRequired   = "Closing an item needs a reason, and none was given."
	whyProtectedClose   = "This item holds a direct message from a person the owner protects, so an agent may comment on it but not close it."
	whyProtectedRule    = "This sender is a person the owner protects, so no rule may drop or close their mail."
	whyApprovalOnly     = "Only the owner can approve or dismiss this proposal, on his own page; an agent cannot."
	whyBlockedSkip      = "This sender or domain is on the owner's list of blocked targets, so no rule may close its mail."
	whyBlockedReply     = "Someone on this thread is on the owner's list of blocked targets, so no reply is proposed or sent to it."
	whySameOwn          = "The newest message in this thread is the owner's own, so nothing in it waits for an answer."
	whySameNoID         = "The message to answer has no Message-ID, so a reply could not be placed in its thread."
	whySameNoRef        = "The message to answer has no Outlook reference, so a reply could not be placed in its thread."
	whySameNoOne        = "The message to answer names nobody to reply to, so there is no one to send it to."
	whySameRecipients   = "A reply may go only to people already on the message it answers, and never to the owner himself."
	whySendOnceAnswered = "This message was already answered from ONE, so it is not answered a second time."
	whySendOnceSending  = "This reply was already approved and is being sent or was sent, so it is not sent a second time."
	whyActorAgent       = "This action is for agents; the owner does it on his own page."
	whyActorOwner       = "Only the owner can approve or dismiss a proposal, and only on his own page, never through an agent's channel."
	whyOnlyOwner        = "This address is for the owner's own page; an agent's channel cannot use it."
	whyApprovalRequired = "Switching Always show me off needs the owner's Approve, so an agent can only propose it."
)

// refuse is a refusal by a guard, with the sentence that says why.
func refuse(guard, why string) *refusal { return &refusal{Guard: guard, Why: why} }
