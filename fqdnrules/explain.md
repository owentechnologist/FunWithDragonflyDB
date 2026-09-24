# How Our Address-Matching Machine Works

## The one question this machine answers

Say a letter shows up. It's from `console.corp.dragonfly.com`, it wants to
use door number `443`, and it's trying to reach `retailer.com`. Our machine
has exactly one job: answer yes or no, is there a rule that allows this
letter through, and do it in less than a thousandth of a second.

That's it. One question, asked hundreds of thousands of times every second,
answered almost instantly. Here's how.

## What we remember: rule cards in labeled boxes

We don't remember every possible letter that could ever be sent. We only
remember the rules someone actually wrote down. A rule looks like an index
card:

```
Who's asking:      *.corp.dragonfly.com
Which door:        443
Where they're going: retailer.com
Rule name:         rule:corp-to-retailer
```

The `*` means "anyone in this family," the same way a party invitation
might say "any kid from the Smith family is welcome," instead of naming
one specific kid.

We keep these cards in boxes. Each box has a name tag made from who's
asking and which door, and inside the box, the cards are sorted by where
they're going. So the box named `*.corp.dragonfly.com : 443` might hold two
cards: one for `retailer.com` exactly, and one for `*.com` (any address
ending in `.com`).

```
BOX: *.corp.dragonfly.com : 443
  retailer.com  ->  rule:corp-to-retailer
  *.com         ->  rule:corp-to-any-dot-com
```

## Two rulebooks for wildcards

A `*` rule can mean two different things, and the person writing the rule
gets to pick:

- **Just my little brother** (single-level): `*.corp.dragonfly.com`
  matches `console.corp.dragonfly.com`, but not
  `a.console.corp.dragonfly.com`. Exactly one name, no more.
- **My whole family tree, forever** (multi-level): `*.corp.dragonfly.com`
  matches `console.corp.dragonfly.com`, `a.console.corp.dragonfly.com`,
  and every descendant after that, no matter how deep.

Both rulebooks use the exact same boxes. Nothing gets copied or stored
twice. Picking a rulebook only changes which boxes we bother to check.

## How we answer the question fast

When a letter shows up, we don't open every box in the building. We walk
one path, from most specific to least specific, and stop at the very first
box that has an answer:

```
console.corp.dragonfly.com    <- is it exactly me?
*.corp.dragonfly.com          <- am I someone's kid in the corp family?
*.dragonfly.com               <- (only checked in the "whole family tree" rulebook)
*.com                         <- does anyone with a .com last name get in?
*                              <- does literally anyone get in?
```

We check the same kind of path for where the letter is going, inside
whichever box we land in. First match, most specific one, wins. As soon as
we find it, we stop, we never keep looking after that.

## How big does the box need to be?

Here's the part people get wrong first: there could be hundreds of
millions of *possible* addresses out there, but we don't store an entry
for every possible address. We only store the *rules* someone actually
wrote. Right now that's tens of millions of rule cards, not hundreds of
millions.

One rule card takes about 125 bytes, roughly the size of this sentence.

| number of rule cards | space needed |
|---|---|
| 1 million | about 125 MB |
| 10 million | about 1.25 GB |
| 50 million | about 6.25 GB |
| 100 million | about 12.5 GB |

12.5 GB is small enough to fit comfortably on one ordinary computer, with
lots of room left over.

**Why not just store an answer for every possible letter instead?** Because
that number gets impossible fast. Imagine 200 million possible addresses,
each with just 3 possible doors and 5 possible destinations. That's
200,000,000 x 3 x 5 = 3 billion combinations, and at 125 bytes each, that's
already 375 GB, for a made-up example far smaller than the real world.
Nobody could store that, and nobody needs to. Storing rules and checking
each letter against them, instead of storing an answer for every letter
that might ever exist, is the whole trick.

## Why it's faster than you can blink

A single human blink takes about a third of a second. This machine answers
in under a thousandth of a second, about 300 times faster than a blink.
Four things make that possible:

1. **Everything lives in memory, not on a disk.** Dragonfly keeps all the
   rule cards in RAM. That's like grabbing a toy from your own room
   instead of waiting for someone to mail it to you.
2. **We never search every box.** The name tag on each box tells us
   exactly where to look, so we go straight to the right few boxes instead
   of checking all of them.
3. **We only ever open at most 3 boxes**, and we ask for all of them in
   one single trip, not one trip per box.
4. **Dragonfly is built to answer exactly this kind of question, over and
   over, as fast as physically possible**, which is the whole reason we
   picked it.

Put together: one round trip, a handful of boxes, all in memory. That's
what keeps every single lookup under one millisecond, even while the
machine is answering 100,000 of these questions every second.

## What this machine can, and can't, answer

**It can answer, almost instantly:** "Is this exact letter, from this
exact address, through this exact door, to this exact destination,
allowed?" It can do that whether there are a thousand rules or a hundred
million rules, because the number of boxes it checks never grows with the
total number of rules, only with how deep the addresses are.

**It can answer under either rulebook:** strict ("just my little
brother") or generous ("my whole family tree"), for the same stored rules,
just by asking the question a different way.

**It can't list every rule that matches.** It stops at the very first,
most specific match on purpose, because that's what makes it fast. If you
needed "show me every rule that could apply," that would be a different,
slower question.

**It can't answer unrelated questions.** It isn't a general-purpose
database. It does one job, matching one address-door-address triple
against a rulebook, and it does that one job about as fast as a computer
can do anything.
