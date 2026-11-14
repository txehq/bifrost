#!/usr/bin/env ruby
# frozen_string_literal: true

# Tests for the pin lookup of check-action-runtimes.rb (TXE-7112). No network: http_get is
# replaced by a table of answers keyed on whether the request carried a token.
#
#   ruby .github/workflows/scripts/check-action-runtimes.test.rb

require 'minitest/autorun'
require 'stringio'
require_relative 'check-action-runtimes'

FakeResponse = Struct.new(:code) do
  def is_a?(klass)
    klass == Net::HTTPSuccess ? code == '200' : super
  end
end

$answers = {}
$requests = []

def http_get(uri, headers = {})
  kind = headers.key?('Authorization') ? :token : :anonymous
  $requests << [uri, kind]
  FakeResponse.new($answers.fetch(kind))
end

class PinLookupTest < Minitest::Test
  SHA = 'a' * 40

  def setup
    PIN_COMMIT_CACHE.clear
    UNVERIFIED_PINS.clear
    $requests.clear
    @token = ENV['GITHUB_TOKEN']
    ENV['GITHUB_TOKEN'] = 'test-token'
  end

  def teardown
    ENV['GITHUB_TOKEN'] = @token
  end

  def occurrence
    Occurrence.new(uses: "owner/repo/setup@#{SHA}", action_repo: 'owner/repo', ref: SHA)
  end

  def kinds
    $requests.map(&:last)
  end

  def test_a_commit_the_token_can_see_passes_in_one_request
    $answers = { token: '200' }
    verify_pin_is_commit(occurrence)
    assert_equal [:token], kinds
    assert_empty UNVERIFIED_PINS
  end

  def test_a_refused_token_is_retried_without_it_and_passes
    %w[401 403 429].each do |refusal|
      setup
      $answers = { token: refusal, anonymous: '200' }
      verify_pin_is_commit(occurrence)
      assert_equal %i[token anonymous], kinds, "HTTP #{refusal}"
      assert_empty UNVERIFIED_PINS
    end
  end

  def test_a_sha_that_is_no_commit_fails
    %w[404 422].each do |answer|
      setup
      $answers = { token: answer }
      error = assert_raises(CheckFailure) { verify_pin_is_commit(occurrence) }
      assert_includes error.message, "HTTP #{answer}"
      assert_equal [:token], kinds
    end
  end

  def test_a_sha_that_is_no_commit_fails_after_the_token_was_refused
    $answers = { token: '403', anonymous: '422' }
    error = assert_raises(CheckFailure) { verify_pin_is_commit(occurrence) }
    assert_includes error.message, 'HTTP 422'
    assert_empty UNVERIFIED_PINS
  end

  def test_a_server_error_fails
    $answers = { token: '500' }
    assert_raises(CheckFailure) { verify_pin_is_commit(occurrence) }
  end

  def test_a_lookup_refused_both_ways_is_reported_by_name_and_does_not_fail
    $answers = { token: '403', anonymous: '403' }
    verify_pin_is_commit(occurrence)
    assert_equal ["owner/repo@#{SHA} (HTTP 403)"], UNVERIFIED_PINS
    out = StringIO.new
    report_unverified_pins(out)
    assert_match(/\A::warning::Pin not checked: .*owner\/repo@a{40} \(HTTP 403\).*NOT verified/, out.string)
  end

  def test_without_a_token_a_refusal_is_not_asked_twice
    ENV['GITHUB_TOKEN'] = ''
    saved = ENV.delete('GH_TOKEN')
    $answers = { anonymous: '403' }
    verify_pin_is_commit(occurrence)
    assert_equal [:anonymous], kinds
    assert_equal 1, UNVERIFIED_PINS.length
  ensure
    ENV['GH_TOKEN'] = saved if saved
  end
end
