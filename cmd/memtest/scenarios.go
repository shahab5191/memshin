package main

import "strings"

// Turn is one exchange: the user prompt and the assistant response that would
// have been generated for it.
type Turn struct {
	Prompt   string
	Response string
}

// Scenario is one scripted conversation with a distinct user id, so the
// harness can run several in parallel and the operator can filter by user.
type Scenario struct {
	UserID string
	Title  string
	Turns  []Turn
}

// parseScript turns a script of alternating "U: ..." / "A: ..." lines into
// turns. Blank lines are ignored.
func parseScript(s string) []Turn {
	var turns []Turn
	var cur Turn
	var havePrompt bool

	flush := func() {
		if havePrompt {
			turns = append(turns, cur)
			cur = Turn{}
			havePrompt = false
		}
	}

	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "U: "):
			flush()
			cur = Turn{Prompt: strings.TrimPrefix(line, "U: ")}
			havePrompt = true
		case strings.HasPrefix(line, "A: "):
			cur.Response = strings.TrimPrefix(line, "A: ")
			turns = append(turns, cur)
			cur = Turn{}
			havePrompt = false
		}
	}
	flush()
	return turns
}

// scenarios returns the built-in scripted conversations. Each is a distinct
// user with a natural topical arc; two of them drift topic mid-way so focus
// keep/replace behaviour is observable.
func scenarios() []Scenario {
	return []Scenario{
		{UserID: "alice_memory", Title: "Building the memshin memory system", Turns: parseScript(aliceMemory)},
		{UserID: "bob_travel", Title: "Planning a trip to Japan", Turns: parseScript(bobTravel)},
		{UserID: "carol_recipe", Title: "Perfecting sourdough then starting a food blog", Turns: parseScript(carolRecipe)},
	}
}

const aliceMemory = `
U: I want my assistant to actually remember our conversations instead of starting from zero each time.
A: That's a common pain point. The usual fix is a short-term buffer, but it gets unwieldy fast.
U: Right, so I'm designing a layered memory system. Short-term for the live thread, then something longer-term.
A: Good instinct. Keeping raw turns for a window and promoting older ones into summaries is the classic shape.
U: I'm calling it memshin. The short-term layer keeps the last few exchanges verbatim.
A: Makes sense. Verbatim is cheap and lossless; the cost is you can't keep it all forever.
U: So I need a promotion step that moves old messages out of short-term into a mid-term store.
A: Exactly. You release bounded batches and summarise them, so mid-term grows slowly.
U: I'm using Postgres with the pgvector extension for the mid-term store.
A: pgvector is a solid choice if you're already on Postgres; one database to operate.
U: Each released batch gets summarised into atomic facts and embedded for retrieval.
A: Atomic facts recall better than a single paragraph, since each one stands alone.
U: The summariser is a Gemini call that returns JSON with a facts array.
A: Structured output keeps parsing trivial and avoids markdown fences leaking into memory.
U: I keep a publish_version on each released batch so a replay never duplicates facts.
A: Versioning plus an idempotent write keyed by user and version is the standard fencing trick.
U: There's also a focus layer — a one-sentence subject of the current conversation.
A: Focus is useful as the retrieval query, so you recall facts relevant to the topic, not just the prompt.
U: Focus updates asynchronously after each response, off the request path.
A: Async keeps latency off the user's turn; you just need a single-flight guard so old extractions don't race.
U: I'm worried a fast typist could cancel an in-flight focus extraction and lose a step.
A: That's the trade-off: coalescing per user means at most one extraction, but rapid turns supersede each other.
U: The window focus reads is the last four exchanges plus the previous subject.
A: That sliding window is what lets focus stay current without seeing the whole history.
U: I added a keep/replace decision so focus only rewrites when the subject genuinely changes.
A: Stability matters; a focus that flips every turn is useless for retrieval scoping.
U: The focus history is append-only, one row per distinct subject with the turn it changed.
A: Append-only gives you a free timeline of topic drift, which is handy for long-term distillation later.
U: Mid-term retrieval embeds focus plus the current prompt and searches the vector store.
A: Scoping the query that way pulls back facts about the topic rather than noise from a terse prompt.
U: I cap retrieval at five facts and drop anything below a cosine similarity of 0.3.
A: Reasonable defaults. The threshold is where you'll spend time tuning on real conversations.
U: I'm testing this manually by talking to the bot, which is slow and hard to reproduce.
A: You want a deterministic harness that drives the pipeline with scripted turns against an ephemeral DB.
U: That's the plan — a separate test database per run so my real tables are untouched.
A: Good. Migrate a fresh DB, run scripted scenarios, and inspect focus and mid-term output after.
U: I want the conversation injected turn by turn, not all at once, so focus sees each window.
A: Right, otherwise only the final window would ever be extracted and every intermediate topic shift is lost.
U: The harness will orchestrate synchronously: append a turn, run focus, release and summarise mid-term.
A: Calling the layers inline in production order removes the race and the sleeps entirely.
U: I'm sketching the promotion state machine: pending, published, processing, promoted.
A: And a reclaim for rows stuck in processing past a lease. That's your recovery path.
U: Batches release only when there are at least sixteen pending messages, eight above the floor.
A: That keeps mid-term from being handed a couple of messages per turn and never an unbounded backlog.
U: Claiming is atomic oldest-first, so two workers never summarise the same batch.
A: The database claim is your exactly-once guard; the worker count is just a throughput knob.
U: I want to run several scripted users in parallel, each with their own topic.
A: Fine, since the layers are keyed per user and the pool is concurrency-safe.
U: One scenario will drift topic mid-way so I can watch focus decide to replace the subject.
A: That's the case that exercises keep versus replace and shows whether the focus prompt is stable enough.
U: Let me also think about how facts should name entities explicitly, never with pronouns.
A: Self-contained facts recall correctly in isolation; pronouns depend on context that isn't stored with the fact.
U: I'll return an empty facts array when a batch has nothing worth remembering.
A: Then the layer stores nothing and just acknowledges the batch, which is the right no-op.
U: Long-term memory is future work — distilling durable facts from mid-term during idle periods.
A: Keep it out of this iteration. Long-term has its own dedup and consolidation problems.
U: I also need embeddings to use matching task types on store and search.
A: Yes, RETRIEVAL_DOCUMENT for storage and RETRIEVAL_QUERY for search, or similarity comparisons are meaningless.
U: The embedding dimension is pinned to 768 to match the migration column.
A: If you ever swap the embedding model you'll have to bump the vector column dimension too.
U: I'm fixing the fact index so a batch can produce several facts per release.
A: Good, the composite key becomes user, version, fact index so each fact is its own retrievable row.
U: Okay, I think the design is solid. Let me switch gears and think about a conference talk.
A: Sure, what's the talk about?
U: I'm proposing a talk on layered memory for LLM assistants for a systems conference.
A: That maps directly to what you've been building; you already have the architecture to present.
U: The abstract should frame the promotion state machine as the core contribution.
A: Focus on exactly-once summarisation and the version fencing; that's the part reviewers find novel.
U: I need to trim it to 300 words and hit the submission deadline next week.
A: Lead with the problem, then the three-layer design, then the batching and recovery results.
U: I'll prepare a demo showing focus drifting across topics in real time.
A: A live demo with visible focus subjects changing is more persuasive than static slides.
U: Great, so the deliverable for this week is the abstract plus the harness.
A: That's a clear plan. Want to draft the abstract outline next?
U: Yes, start with a one-line thesis about why naive context windows fail.
A: "Context windows are lossy and unbounded; layered memory turns raw turns into durable, scoped facts."
U: Perfect, then list the three layers and their responsibilities.
A: Short-term window, mid-term vector facts, focus subject — each with one responsibility.
U: I should mention the ephemeral test harness as part of the evaluation method.
A: Including it strengthens the evaluation section by showing reproducibility.
U: Alright, I have what I need to write it up. Thanks for thinking through this with me.
A: Anytime. Good luck with the submission.
`

const bobTravel = `
U: I'm planning a two-week trip to Japan in the spring and I want to get it right.
A: Great time to go. Which cities are you thinking about?
U: Definitely Tokyo and Kyoto, and I'm considering a few days in Osaka.
A: That's the classic first-trip triangle. Two weeks gives you enough time without rushing.
U: I want to spend about a week in Tokyo to soak it in.
A: A week is good. You can do day trips to Nikko or Kamakura without changing hotels.
U: Then maybe four days in Kyoto for the temples and shrines.
A: Four days in Kyoto is about right for the highlights plus a day trip to Nara.
U: Osaka would be two or three days at the end, mostly for food.
A: Osaka is worth it for the street food alone, and it's a short hop from Kyoto.
U: I'm trying to budget. Flights look around a thousand dollars round trip.
A: That's typical from the US in spring, though cherry blossom season pushes prices up.
U: I'd rather go in late May to avoid the crowd and the peak prices.
A: Late May has mild weather and thinner crowds. You miss the blossoms but save money.
U: For hotels, I'm seeing business hotels around eighty dollars a night in Tokyo.
A: Business hotels are compact but clean and well-located near stations, which matters more than size.
U: I'd like a ryokan stay for at least one night in Kyoto for the experience.
A: A ryokan with a kaiseki dinner is a splurge but a memorable part of the trip.
U: What's a reasonable daily food budget if I mostly eat local?
A: You can eat well on thirty to fifty dollars a day if you stick to convenience stores and casual spots.
U: I want to try conveyor belt sushi and a proper sushi counter too.
A: Do both. Conveyor belt for fun and volume, one good counter for the experience.
U: I need to figure out the rail pass and whether it's worth it for my route.
A: With Tokyo, Kyoto and Osaka, the pass usually breaks even. Calculate point-to-point before committing.
U: I'll book a pocket wifi or an eSIM so I can navigate and translate.
A: An eSIM is simpler than a pocket device; make sure your phone is unlocked.
U: I want to learn a few Japanese phrases before I go.
A: Start with greetings, thank you, and asking where the station is. Locals appreciate the effort.
U: I've always wanted to learn Japanese properly, not just tourist phrases.
A: Then this trip could be the motivation. Even an hour a day before you go helps.
U: Maybe I should sign up for a structured course instead of self-teaching.
A: A course gives you accountability and a tutor to correct pronunciation early, which is worth a lot.
U: I found a beginner class that meets twice a week near my office.
A: Twice a week is a good cadence; daily review between classes matters more than the class time.
U: The class starts with hiragana and katakana, which feels like a wall of symbols.
A: It's just memorisation. Drill both syllabaries for two weeks and it stops looking like noise.
U: I downloaded a flashcard app to practice kana and basic vocabulary.
A: Spaced repetition is exactly right for vocabulary. Ten minutes daily beats a Sunday cram.
U: I'm surprised how quickly I can read simple signs now that I know kana.
A: It unlocks menus and station signs, which makes the trip feel far less intimidating.
U: I still freeze when I try to form a sentence out loud.
A: That's normal. Practise set phrases until they're automatic, then expand slowly.
U: I'll focus on ordering food and asking prices since I'll use those constantly.
A: Those are high-frequency and low-stakes, so they're perfect to build confidence with.
U: My tutor says pitch accent matters and I should pay attention early.
A: It's easy to ignore and hard to fix later. Shadowing audio from native speakers helps.
U: I've been shadowing short dialogues on my commute.
A: That's a great habit. It trains both rhythm and accent without extra study time.
U: I'm thinking of doing a language exchange with a Japanese partner online.
A: Good idea. It turns passive study into real conversation and gives you cultural context too.
U: So the plan is: class twice a week, daily kana and vocab, and a weekly exchange.
A: That's a sustainable routine. Consistency over a few months will show real progress.
U: Back to the trip — I need to decide on the rough day-by-day itinerary.
A: Let's block it out. Days one to seven Tokyo, eight to eleven Kyoto, twelve to fourteen Osaka.
U: I'll put a day trip to Hakone in the Tokyo week to see Mount Fuji.
A: Hakone is a nice break from the city; check the weather and go on the clearest day.
U: In Kyoto I want to do the Fushimi Inari hike early in the morning.
A: Go at sunrise before the crowds. The torii gates are empty then and it's cooler.
U: I'll keep the itinerary loose so I can wander and follow recommendations.
A: Leave slack every day. The best parts of a trip like this are unplanned.
U: I'm also mapping out the cost so I don't blow the budget on the ryokan.
A: Treat the ryokan as the one big splurge and keep other nights modest; it balances out.
U: I think the total will land around four thousand dollars all in.
A: That's a realistic all-in for two weeks including flights, hotels and a rail pass.
U: I'll start booking flights this week since prices only climb closer to the date.
A: Book flights soon, and lock the ryokan early since the good ones fill up.
U: Thanks, this has been really helpful. I feel ready to commit.
A: Glad to help. Enjoy the planning and the trip.
`

const carolRecipe = `
U: I've been trying to bake sourdough and my loaves keep coming out flat and dense.
A: Flat loaves usually mean a weak starter or under-proofing. How old is your starter?
U: I started it about ten days ago and it doubles but slowly.
A: At ten days it's still young. Feed it twice a day and it should get more vigorous within a week.
U: I feed it equal parts flour and water by weight, once a day.
A: Try a 1:2:2 ratio and twice a day for a few days; that wakes up a sluggish starter.
U: My kitchen is cold, around eighteen degrees, which might be slowing it down.
A: Cold slows fermentation a lot. Keep the starter somewhere around twenty-four degrees if you can.
U: I'll try proofing it in the oven with the light on.
A: That's a common trick and gives a steady warm spot, but monitor the temperature so it doesn't run hot.
U: My dough seems fine after the first rise but loses its shape when I turn it out.
A: That suggests weak gluten development. More stretch and folds during bulk fermentation helps.
U: I do two sets of folds. Should I do more?
A: Do four sets spaced thirty minutes apart in the first two hours, then leave it alone.
U: I'll also try a higher protein bread flour instead of all-purpose.
A: Bread flour makes a big difference; it develops a stronger network that holds the rise.
U: How do I know when the dough is properly proofed?
A: Poke it gently. If the indent springs back slowly and leaves a slight mark, it's ready.
U: I over-proofed a loaf once and it spread like a pancake in the oven.
A: Over-proofed dough has exhausted its structure. Use the poke test and catch it before that point.
U: I bake in a Dutch oven at 230 degrees, which has helped the crust.
A: A covered Dutch oven traps steam for the oven spring; bake covered for twenty minutes then uncover.
U: My crust is great but the crumb has big holes with dense patches between them.
A: That's under-mixing or a weak starter. Mix thoroughly at the start and build more strength.
U: I'll try an autolyse step where I mix flour and water and rest before adding starter.
A: Autolyse improves gluten and flavour. Rest for forty-five minutes before adding salt and starter.
U: I scored the top and it still burst out the side on my last bake.
A: Score deeper at a shallow angle, and make sure there's enough steam early in the bake.
U: Honestly, I think my starter might just be too weak still.
A: Keep feeding it and give it another week. Patience is the hardest ingredient in sourdough.
U: I'm also thinking of trying a rye starter for more flavour.
A: Rye starters are very active and add a nice tang. You can convert a portion of your existing starter.
U: Would I need to change the flour ratios in the final loaf?
A: Not much. You can keep the loaf mostly wheat and just feed the starter rye.
U: I've been keeping a notebook of every bake with times and temperatures.
A: That's the right instinct. Sourdough is about noticing patterns across bakes.
U: People keep asking me for the recipe, which made me think about sharing this more widely.
A: You could start a blog or an Instagram account documenting the process.
U: I'm leaning towards a food blog with detailed posts and photos of each loaf.
A: A blog lets you write proper recipes with notes, which a photo feed can't capture.
U: I'd need to get better at food photography to make it appealing.
A: Natural light and a plain background go a long way. You don't need fancy gear to start.
U: I have an old DSLR I could use for the crumb shots.
A: A DSLR with a 50mm lens is perfect for food. Shoot near a window for soft light.
U: What should I write about first — a beginner's guide or my sourdough journey?
A: A beginner's guide has more search value and pulls people in, then your story keeps them.
U: I'll document my starter from scratch with photos at each stage.
A: Great format. Readers love watching a starter come to life day by day.
U: I could also turn it into a newsletter so people get a bake update weekly.
A: A newsletter builds a loyal audience over time. Keep it short and useful.
U: This is becoming a real project, not just a hobby.
A: Hobbies turn into projects when you start documenting. Run with it and see where it goes.
U: I need to come up with a name for the blog.
A: Something short and evocative, tied to bread or your kitchen. Test a few on friends.
U: Maybe I'll call it "The Slow Loaf" to capture the patience angle.
A: "The Slow Loaf" is memorable and fits the sourdough philosophy perfectly.
U: I'll set up the site this weekend and write the first post.
A: Start with a simple platform and don't overthink the design. Content first.
U: Thanks, I'm excited to actually publish something instead of just baking in private.
A: That's the fun part. The bread only gets better when you share what you learn.
`
