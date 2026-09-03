use std::collections::BTreeSet;

use chrono::{DateTime, Datelike, Duration, Local, Timelike, Utc};
use chrono_tz::Tz;
use rusqlite::{OptionalExtension, Row, params};

use super::{
    AUDIENCE_CONVERSATION, CONTENT_SCHEDULED_REMINDER, CONTENT_TEXT, ConversationChunk, StateError,
    StateTx, Store, decode_time, encode_time,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ScheduleKind {
    At,
    Every,
    Cron,
}

impl ScheduleKind {
    fn as_str(self) -> &'static str {
        match self {
            Self::At => "at",
            Self::Every => "every",
            Self::Cron => "cron",
        }
    }

    fn parse(value: &str) -> Result<Self, StateError> {
        match value {
            "at" => Ok(Self::At),
            "every" => Ok(Self::Every),
            "cron" => Ok(Self::Cron),
            _ => Err(StateError::Validation(format!(
                "unsupported schedule kind {value:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReminderSchedule {
    pub kind: ScheduleKind,
    pub at: Option<DateTime<Utc>>,
    pub every_ms: i64,
    pub anchor_at: Option<DateTime<Utc>>,
    pub cron_expr: String,
    pub timezone: String,
}

impl ReminderSchedule {
    pub fn at(at: DateTime<Utc>) -> Self {
        Self {
            kind: ScheduleKind::At,
            at: Some(at),
            every_ms: 0,
            anchor_at: None,
            cron_expr: String::new(),
            timezone: String::new(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reminder {
    pub id: i64,
    pub channel_id: String,
    pub sender_id: String,
    pub message: String,
    pub schedule: ReminderSchedule,
    pub fire_at: DateTime<Utc>,
    pub enabled: bool,
}

pub fn next_reminder_run(
    schedule: &ReminderSchedule,
    now: DateTime<Utc>,
) -> Result<DateTime<Utc>, StateError> {
    match schedule.kind {
        ScheduleKind::At => schedule
            .at
            .filter(|at| *at > now)
            .ok_or_else(|| StateError::Validation("at must be in the future".into())),
        ScheduleKind::Every => {
            if schedule.every_ms < 1 {
                return Err(StateError::Validation(
                    "every_ms must be a positive integer".into(),
                ));
            }
            let anchor = schedule.anchor_at.unwrap_or(now);
            if now < anchor {
                return Ok(anchor);
            }
            let elapsed = now.signed_duration_since(anchor).num_milliseconds();
            let steps = elapsed
                .checked_div(schedule.every_ms)
                .and_then(|steps| steps.checked_add(1))
                .ok_or_else(|| StateError::Validation("every_ms is too large".into()))?;
            let advance_ms = schedule
                .every_ms
                .checked_mul(steps)
                .ok_or_else(|| StateError::Validation("every_ms is too large".into()))?;
            anchor
                .checked_add_signed(Duration::milliseconds(advance_ms))
                .ok_or_else(|| StateError::Validation("every_ms is too large".into()))
        }
        ScheduleKind::Cron => next_cron_run(schedule, now),
    }
}

pub(crate) fn validate_standard_cron(expression: &str) -> Result<(), StateError> {
    CronSpec::parse_standard(expression).map(|_| ())
}

impl StateTx<'_> {
    pub fn add_reminder(
        &self,
        channel_id: &str,
        sender_id: &str,
        message: &str,
        schedule: &ReminderSchedule,
        fire_at: DateTime<Utc>,
    ) -> Result<i64, StateError> {
        self.transaction.execute(
            "INSERT INTO reminders (channel_id, sender_id, message, fire_at, schedule_kind, every_ms, anchor_at, cron_expr, timezone, enabled) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, 1)",
            params![
                channel_id,
                sender_id,
                message,
                encode_time(fire_at),
                schedule.kind.as_str(),
                schedule.every_ms,
                schedule.anchor_at.map(encode_time),
                schedule.cron_expr,
                schedule.timezone,
            ],
        )?;
        Ok(self.transaction.last_insert_rowid())
    }

    pub fn list_reminders(
        &self,
        channel_id: &str,
        sender_id: &str,
    ) -> Result<Vec<Reminder>, StateError> {
        let mut statement = self.transaction.prepare(&format!(
            "SELECT {REMINDER_COLUMNS} FROM reminders WHERE channel_id=?1 AND sender_id=?2 ORDER BY julianday(fire_at) ASC"
        ))?;
        let raw = statement
            .query_map(params![channel_id, sender_id], raw_reminder)?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter().map(Reminder::try_from).collect()
    }

    pub fn get_reminder_for_user(
        &self,
        id: i64,
        channel_id: &str,
        sender_id: &str,
    ) -> Result<Reminder, StateError> {
        let raw = self
            .transaction
            .query_row(
                &format!("SELECT {REMINDER_COLUMNS} FROM reminders WHERE id=?1 AND channel_id=?2 AND sender_id=?3"),
                params![id, channel_id, sender_id],
                raw_reminder,
            )
            .optional()?
            .ok_or_else(|| StateError::Validation(format!("reminder {id} not found")))?;
        raw.try_into()
    }

    pub fn update_reminder_for_user(&self, reminder: &Reminder) -> Result<(), StateError> {
        let changed = self.transaction.execute(
            "UPDATE reminders SET message=?1, fire_at=?2, schedule_kind=?3, every_ms=?4, anchor_at=?5, cron_expr=?6, timezone=?7, enabled=?8 WHERE id=?9 AND channel_id=?10 AND sender_id=?11",
            params![
                reminder.message,
                encode_time(reminder.fire_at),
                reminder.schedule.kind.as_str(),
                reminder.schedule.every_ms,
                reminder.schedule.anchor_at.map(encode_time),
                reminder.schedule.cron_expr,
                reminder.schedule.timezone,
                reminder.enabled,
                reminder.id,
                reminder.channel_id,
                reminder.sender_id,
            ],
        )?;
        if changed != 1 {
            return Err(StateError::Validation(format!(
                "reminder {} not found",
                reminder.id
            )));
        }
        Ok(())
    }

    pub fn delete_reminder_for_user(
        &self,
        id: i64,
        channel_id: &str,
        sender_id: &str,
    ) -> Result<(), StateError> {
        let changed = self.transaction.execute(
            "DELETE FROM reminders WHERE id=?1 AND channel_id=?2 AND sender_id=?3",
            params![id, channel_id, sender_id],
        )?;
        if changed != 1 {
            return Err(StateError::Validation(format!("reminder {id} not found")));
        }
        Ok(())
    }

    fn complete_reminder(&self, reminder: &Reminder, now: DateTime<Utc>) -> Result<(), StateError> {
        if reminder.schedule.kind == ScheduleKind::At {
            let changed = self
                .transaction
                .execute("DELETE FROM reminders WHERE id=?1", [reminder.id])?;
            if changed != 1 {
                return Err(StateError::Validation(format!(
                    "reminder {} not found",
                    reminder.id
                )));
            }
            return Ok(());
        }
        let next = next_reminder_run(&reminder.schedule, now)?;
        let changed = self.transaction.execute(
            "UPDATE reminders SET fire_at=?1 WHERE id=?2 AND fire_at=?3",
            params![
                encode_time(next),
                reminder.id,
                encode_time(reminder.fire_at)
            ],
        )?;
        if changed != 1 {
            return Err(StateError::Validation(format!(
                "reminder {} changed while delivering",
                reminder.id
            )));
        }
        Ok(())
    }
}

impl Store {
    pub fn fetch_due_reminders(&self) -> Result<Vec<Reminder>, StateError> {
        self.fetch_due_reminders_at(Utc::now())
    }

    pub fn fetch_due_reminders_at(&self, now: DateTime<Utc>) -> Result<Vec<Reminder>, StateError> {
        let connection = self.lock()?;
        let mut statement = connection.prepare(&format!(
            "SELECT {REMINDER_COLUMNS} FROM reminders WHERE enabled=1 AND julianday(fire_at) <= julianday(?1) ORDER BY julianday(fire_at) ASC"
        ))?;
        let raw = statement
            .query_map([encode_time(now)], raw_reminder)?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter().map(Reminder::try_from).collect()
    }

    pub fn complete_reminder(
        &self,
        reminder: &Reminder,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        self.with_tx(|tx| tx.complete_reminder(reminder, now))
    }

    pub fn complete_reminder_delivery(
        &self,
        reminder: &Reminder,
        now: DateTime<Utc>,
        scheduled_content: &str,
        notification: &str,
    ) -> Result<(), StateError> {
        self.with_tx(|tx| {
            tx.save_conversation_message(
                &reminder.channel_id,
                &reminder.sender_id,
                "user",
                CONTENT_SCHEDULED_REMINDER,
                AUDIENCE_CONVERSATION,
                scheduled_content,
            )?;
            tx.save_conversation_message(
                &reminder.channel_id,
                &reminder.sender_id,
                "assistant",
                CONTENT_TEXT,
                AUDIENCE_CONVERSATION,
                notification,
            )?;
            tx.complete_reminder(reminder, now)
        })
    }

    #[allow(clippy::too_many_arguments)]
    pub fn complete_reminder_trace_delivery_indexed(
        &self,
        reminder: &Reminder,
        now: DateTime<Utc>,
        scheduled_content: &str,
        notification: &str,
        trace_id: i64,
        delivery_event_id: i64,
        provider_message_id: &str,
        chunks: &[ConversationChunk],
    ) -> Result<(), StateError> {
        self.with_tx(|tx| {
            let start_history_id = tx.save_conversation_message(
                &reminder.channel_id,
                &reminder.sender_id,
                "user",
                CONTENT_SCHEDULED_REMINDER,
                AUDIENCE_CONVERSATION,
                scheduled_content,
            )?;
            let history_id = tx.save_conversation_message(
                &reminder.channel_id,
                &reminder.sender_id,
                "assistant",
                CONTENT_TEXT,
                AUDIENCE_CONVERSATION,
                notification,
            )?;
            if !chunks.is_empty() {
                tx.save_conversation_chunks(start_history_id, history_id, chunks)?;
            }
            tx.complete_reminder(reminder, now)?;
            tx.transaction.execute(
                "UPDATE delivery_attempts SET provider_message_id=?1, conversation_history_id=?2, accepted_at=CURRENT_TIMESTAMP WHERE event_id=?3",
                params![provider_message_id, history_id, delivery_event_id],
            )?;
            tx.transaction.execute(
                "UPDATE trace_events SET status='succeeded', completed_at=CURRENT_TIMESTAMP WHERE id=?1",
                [delivery_event_id],
            )?;
            tx.transaction.execute(
                "UPDATE response_traces SET status='completed', completed_at=CURRENT_TIMESTAMP WHERE id=?1",
                [trace_id],
            )?;
            Ok(())
        })
    }
}

const REMINDER_COLUMNS: &str = "id, channel_id, sender_id, message, fire_at, schedule_kind, every_ms, anchor_at, cron_expr, timezone, enabled";

type RawReminder = (
    i64,
    String,
    String,
    String,
    String,
    String,
    i64,
    Option<String>,
    String,
    String,
    bool,
);

fn raw_reminder(row: &Row<'_>) -> rusqlite::Result<RawReminder> {
    Ok((
        row.get(0)?,
        row.get(1)?,
        row.get(2)?,
        row.get(3)?,
        row.get(4)?,
        row.get(5)?,
        row.get(6)?,
        row.get(7)?,
        row.get(8)?,
        row.get(9)?,
        row.get(10)?,
    ))
}

impl TryFrom<RawReminder> for Reminder {
    type Error = StateError;

    fn try_from(raw: RawReminder) -> Result<Self, Self::Error> {
        let fire_at = decode_time(raw.4)?;
        Ok(Self {
            id: raw.0,
            channel_id: raw.1,
            sender_id: raw.2,
            message: raw.3,
            schedule: ReminderSchedule {
                kind: ScheduleKind::parse(&raw.5)?,
                at: Some(fire_at),
                every_ms: raw.6,
                anchor_at: raw.7.map(decode_time).transpose()?,
                cron_expr: raw.8,
                timezone: raw.9,
            },
            fire_at,
            enabled: raw.10,
        })
    }
}

#[derive(Debug)]
struct CronSpec {
    seconds: Field,
    minutes: Field,
    hours: Field,
    days: Field,
    months: Field,
    weekdays: Field,
    has_seconds: bool,
    every: Option<Duration>,
}

#[derive(Debug)]
struct Field {
    allowed: BTreeSet<u32>,
    wildcard: bool,
}

fn next_cron_run(
    schedule: &ReminderSchedule,
    now: DateTime<Utc>,
) -> Result<DateTime<Utc>, StateError> {
    let spec = CronSpec::parse(schedule.cron_expr.trim())?;
    let timezone = if schedule.timezone.trim().is_empty() || schedule.timezone == "Local" {
        None
    } else {
        Some(schedule.timezone.parse::<Tz>().map_err(|error| {
            StateError::Validation(format!(
                "invalid IANA timezone {:?}: {error}",
                schedule.timezone
            ))
        })?)
    };
    if let Some(delay) = spec.every {
        let delay = if delay < Duration::seconds(1) {
            Duration::seconds(1)
        } else {
            Duration::seconds(delay.num_seconds())
        };
        return now
            .checked_add_signed(delay - Duration::nanoseconds(now.timestamp_subsec_nanos().into()))
            .ok_or_else(|| StateError::Validation("cron duration is too large".into()));
    }

    let mut candidate = if spec.has_seconds {
        now.with_nanosecond(0).unwrap() + Duration::seconds(1)
    } else {
        (now + Duration::minutes(1))
            .with_second(0)
            .unwrap()
            .with_nanosecond(0)
            .unwrap()
    };
    let deadline = now + Duration::days(366 * 5);
    while candidate <= deadline {
        let date_and_minute_match = match timezone {
            Some(timezone) => spec.matches_date_and_minute(candidate.with_timezone(&timezone)),
            None => spec.matches_date_and_minute(candidate.with_timezone(&Local)),
        };
        if date_and_minute_match && spec.seconds.allowed.contains(&candidate.second()) {
            return Ok(candidate);
        }
        candidate = if spec.has_seconds && date_and_minute_match && candidate.second() < 59 {
            candidate + Duration::seconds(1)
        } else {
            (candidate + Duration::minutes(1)).with_second(0).unwrap()
        };
    }
    Err(StateError::Validation(
        "cron expression has no future run".into(),
    ))
}

impl CronSpec {
    fn parse(expression: &str) -> Result<Self, StateError> {
        Self::parse_with_optional_seconds(expression, true)
    }

    fn parse_standard(expression: &str) -> Result<Self, StateError> {
        Self::parse_with_optional_seconds(expression, false)
    }

    fn parse_with_optional_seconds(
        expression: &str,
        optional_seconds: bool,
    ) -> Result<Self, StateError> {
        if let Some(value) = expression.strip_prefix("@every ") {
            return Ok(Self::constant_delay(parse_go_duration(value)?));
        }
        let expanded = match expression {
            "@yearly" | "@annually" => "0 0 1 1 *",
            "@monthly" => "0 0 1 * *",
            "@weekly" => "0 0 * * 0",
            "@daily" | "@midnight" => "0 0 * * *",
            "@hourly" => "0 * * * *",
            value if value.starts_with('@') => {
                return Err(StateError::Validation(format!(
                    "invalid cron expression: unrecognized descriptor {value:?}"
                )));
            }
            value => value,
        };
        let fields: Vec<_> = expanded.split_whitespace().collect();
        let (seconds, fields, has_seconds) = match fields.len() {
            5 => (Field::single(0), fields.as_slice(), false),
            6 if optional_seconds => (Field::parse(fields[0], 0, 59, &[])?, &fields[1..], true),
            _ => {
                let expected = if optional_seconds {
                    "five or six fields"
                } else {
                    "exactly five fields"
                };
                return Err(StateError::Validation(format!(
                    "invalid cron expression: expected {expected}"
                )));
            }
        };
        Ok(Self {
            seconds,
            minutes: Field::parse(fields[0], 0, 59, &[])?,
            hours: Field::parse(fields[1], 0, 23, &[])?,
            days: Field::parse(fields[2], 1, 31, &[])?,
            months: Field::parse(fields[3], 1, 12, &MONTH_NAMES)?,
            weekdays: Field::parse(fields[4], 0, 6, &WEEKDAY_NAMES)?,
            has_seconds,
            every: None,
        })
    }

    fn constant_delay(delay: Duration) -> Self {
        Self {
            seconds: Field::single(0),
            minutes: Field::single(0),
            hours: Field::single(0),
            days: Field::single(1),
            months: Field::single(1),
            weekdays: Field::single(0),
            has_seconds: true,
            every: Some(delay),
        }
    }

    fn matches_date_and_minute<T: chrono::TimeZone>(&self, value: DateTime<T>) -> bool {
        let day_match = self.days.allowed.contains(&value.day());
        let weekday_match = self
            .weekdays
            .allowed
            .contains(&value.weekday().num_days_from_sunday());
        let calendar_day_match = if self.days.wildcard || self.weekdays.wildcard {
            day_match && weekday_match
        } else {
            day_match || weekday_match
        };
        self.minutes.allowed.contains(&value.minute())
            && self.hours.allowed.contains(&value.hour())
            && self.months.allowed.contains(&value.month())
            && calendar_day_match
    }
}

impl Field {
    fn single(value: u32) -> Self {
        Self {
            allowed: BTreeSet::from([value]),
            wildcard: false,
        }
    }

    fn parse(value: &str, min: u32, max: u32, names: &[(&str, u32)]) -> Result<Self, StateError> {
        let mut allowed = BTreeSet::new();
        let mut wildcard = false;
        for part in value.split(',') {
            let components: Vec<_> = part.split('/').collect();
            if components.len() > 2 {
                return Err(StateError::Validation(
                    "invalid cron expression: too many slashes".into(),
                ));
            }
            let base = components[0];
            let step: u32 = components
                .get(1)
                .copied()
                .unwrap_or("1")
                .parse()
                .ok()
                .filter(|step| *step > 0)
                .ok_or_else(|| {
                    StateError::Validation("invalid cron expression: invalid step".into())
                })?;
            let (start, end) = if matches!(base, "*" | "?") {
                wildcard |= step == 1;
                (min, max)
            } else if let Some((start, end)) = base.split_once('-') {
                (
                    parse_bound(start, min, max, names)?,
                    parse_bound(end, min, max, names)?,
                )
            } else {
                let value = parse_bound(base, min, max, names)?;
                (value, if components.len() == 2 { max } else { value })
            };
            if start > end {
                return Err(StateError::Validation(
                    "invalid cron expression: reversed range".into(),
                ));
            }
            allowed.extend((start..=end).step_by(step as usize));
        }
        if allowed.is_empty() {
            return Err(StateError::Validation(
                "invalid cron expression: empty field".into(),
            ));
        }
        Ok(Self { allowed, wildcard })
    }
}

fn parse_bound(value: &str, min: u32, max: u32, names: &[(&str, u32)]) -> Result<u32, StateError> {
    names
        .iter()
        .find(|(name, _)| value.eq_ignore_ascii_case(name))
        .map(|(_, value)| *value)
        .or_else(|| value.parse::<u32>().ok())
        .filter(|value| (min..=max).contains(value))
        .ok_or_else(|| StateError::Validation("invalid cron expression: invalid value".into()))
}

const MONTH_NAMES: [(&str, u32); 12] = [
    ("jan", 1),
    ("feb", 2),
    ("mar", 3),
    ("apr", 4),
    ("may", 5),
    ("jun", 6),
    ("jul", 7),
    ("aug", 8),
    ("sep", 9),
    ("oct", 10),
    ("nov", 11),
    ("dec", 12),
];

const WEEKDAY_NAMES: [(&str, u32); 7] = [
    ("sun", 0),
    ("mon", 1),
    ("tue", 2),
    ("wed", 3),
    ("thu", 4),
    ("fri", 5),
    ("sat", 6),
];

fn parse_go_duration(value: &str) -> Result<Duration, StateError> {
    let value = value.trim();
    if value.is_empty() {
        return Err(StateError::Validation(
            "invalid cron expression: empty duration".into(),
        ));
    }
    let mut rest = value;
    let sign = if let Some(value) = rest.strip_prefix('-') {
        rest = value;
        -1.0
    } else {
        rest = rest.strip_prefix('+').unwrap_or(rest);
        1.0
    };
    if rest == "0" {
        return Ok(Duration::zero());
    }
    let mut total_nanos = 0.0_f64;
    let mut parts = 0;
    while !rest.is_empty() {
        let number_end = rest
            .char_indices()
            .take_while(|(_, character)| character.is_ascii_digit() || *character == '.')
            .map(|(index, character)| index + character.len_utf8())
            .last()
            .unwrap_or(0);
        if number_end == 0 {
            return Err(StateError::Validation(
                "invalid cron expression: invalid duration".into(),
            ));
        }
        let amount: f64 = rest[..number_end].parse().map_err(|_| {
            StateError::Validation("invalid cron expression: invalid duration number".into())
        })?;
        rest = &rest[number_end..];
        let (unit, factor) = [
            ("ms", 1_000_000.0),
            ("us", 1_000.0),
            ("µs", 1_000.0),
            ("μs", 1_000.0),
            ("ns", 1.0),
            ("s", 1_000_000_000.0),
            ("m", 60_000_000_000.0),
            ("h", 3_600_000_000_000.0),
        ]
        .into_iter()
        .find(|(unit, _)| rest.starts_with(unit))
        .ok_or_else(|| {
            StateError::Validation("invalid cron expression: unknown duration unit".into())
        })?;
        total_nanos += amount * factor;
        rest = &rest[unit.len()..];
        parts += 1;
    }
    let signed = sign * total_nanos;
    if parts == 0 || !signed.is_finite() || signed > i64::MAX as f64 || signed < i64::MIN as f64 {
        return Err(StateError::Validation(
            "invalid cron expression: duration is out of range".into(),
        ));
    }
    Ok(Duration::nanoseconds(signed as i64))
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn every(every_ms: i64, anchor_at: DateTime<Utc>) -> ReminderSchedule {
        ReminderSchedule {
            kind: ScheduleKind::Every,
            at: None,
            every_ms,
            anchor_at: Some(anchor_at),
            cron_expr: String::new(),
            timezone: String::new(),
        }
    }

    fn cron(expression: &str, timezone: &str) -> ReminderSchedule {
        ReminderSchedule {
            kind: ScheduleKind::Cron,
            at: None,
            every_ms: 0,
            anchor_at: None,
            cron_expr: expression.into(),
            timezone: timezone.into(),
        }
    }

    #[test]
    fn next_run_supports_anchored_every_and_six_field_cron() {
        let now = Utc.with_ymd_and_hms(2026, 7, 15, 0, 0, 0).unwrap();
        assert_eq!(
            next_reminder_run(&every(3_600_000, now - Duration::minutes(30)), now).unwrap(),
            now + Duration::minutes(30)
        );
        assert_eq!(
            next_reminder_run(&every(3_600_000, now + Duration::hours(2)), now).unwrap(),
            now + Duration::hours(2)
        );
        assert_eq!(
            next_reminder_run(&cron("30 0 8 * * *", "Asia/Kuala_Lumpur"), now).unwrap(),
            now + Duration::seconds(30)
        );
    }

    #[test]
    fn cron_uses_configured_timezone_and_weekdays() {
        let now = Utc.with_ymd_and_hms(2026, 7, 15, 0, 0, 0).unwrap();
        assert_eq!(
            next_reminder_run(&cron("0 8 * * 1-5", "Asia/Kuala_Lumpur"), now).unwrap(),
            Utc.with_ymd_and_hms(2026, 7, 16, 0, 0, 0).unwrap()
        );
    }

    #[test]
    fn cron_matches_go_names_steps_wildcards_and_descriptors() {
        let now = Utc.with_ymd_and_hms(2026, 7, 15, 0, 0, 0).unwrap() + Duration::milliseconds(500);
        assert_eq!(
            next_reminder_run(&cron("5/15 8 ? JUL WED", "UTC"), now).unwrap(),
            Utc.with_ymd_and_hms(2026, 7, 15, 8, 5, 0).unwrap()
        );
        assert_eq!(
            next_reminder_run(&cron("@every 1h30m", "UTC"), now).unwrap(),
            Utc.with_ymd_and_hms(2026, 7, 15, 1, 30, 0).unwrap()
        );
        assert!(validate_standard_cron("@monthly").is_ok());
        assert!(validate_standard_cron("0 8 * * MON").is_ok());
        assert!(validate_standard_cron("0 0 8 * * MON").is_err());
        assert!(CronSpec::parse("0 8 * * 7").is_err());
    }

    #[test]
    fn reminder_delivery_is_atomic_and_advances_only_after_success() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let now = Utc.with_ymd_and_hms(2026, 7, 15, 12, 0, 0).unwrap();
        let schedule = cron("0 8 * * *", "UTC");
        let id = store
            .with_tx(|tx| {
                tx.add_reminder(
                    "telegram",
                    "owner",
                    "briefing",
                    &schedule,
                    now - Duration::hours(4),
                )
            })
            .unwrap();
        let reminder = store
            .with_tx(|tx| tx.get_reminder_for_user(id, "telegram", "owner"))
            .unwrap();
        let mut stale = reminder.clone();
        stale.fire_at -= Duration::minutes(1);
        assert!(
            store
                .complete_reminder_delivery(&stale, now, r#"{"reminder_id":1}"#, "delivered")
                .is_err()
        );
        assert!(store.get_all_conversation_history().unwrap().is_empty());

        store
            .complete_reminder_delivery(
                &reminder,
                now,
                r#"{"reminder_id":1}"#,
                "delivered briefing",
            )
            .unwrap();
        let advanced = store
            .with_tx(|tx| tx.get_reminder_for_user(id, "telegram", "owner"))
            .unwrap();
        assert_eq!(
            advanced.fire_at,
            Utc.with_ymd_and_hms(2026, 7, 16, 8, 0, 0).unwrap()
        );
        assert_eq!(store.get_all_conversation_history().unwrap().len(), 2);
    }

    #[test]
    fn one_shot_is_deleted_only_when_completed() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let now = Utc::now();
        let schedule = ReminderSchedule::at(now - Duration::minutes(1));
        let id = store
            .with_tx(|tx| tx.add_reminder("telegram", "owner", "due", &schedule, now))
            .unwrap();
        let reminder = store
            .with_tx(|tx| tx.get_reminder_for_user(id, "telegram", "owner"))
            .unwrap();
        store.complete_reminder(&reminder, now).unwrap();
        assert!(
            store
                .with_tx(|tx| tx.get_reminder_for_user(id, "telegram", "owner"))
                .is_err()
        );
    }
}
