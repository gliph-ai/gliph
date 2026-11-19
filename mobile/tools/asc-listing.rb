#!/usr/bin/env ruby
# Push the App Store listing text from store/metadata.md to App Store Connect.
#
#   ruby mobile/tools/asc-listing.rb state    # what ASC has now
#   ruby mobile/tools/asc-listing.rb push     # subtitle, promo, keywords, description
#
# The text lives in the repo so it is reviewed like anything else and never
# retyped into a web form. Written through the voice-dna skill; run the gate
# before pushing.
#
# ponytail: no screenshots, no age rating, no submission here — those are one
# time each, and lugha's tools/asc.rb already has the shape to copy when Gliph
# actually goes to the store.
require 'jwt'
require 'json'
require 'net/http'
require 'uri'
require 'openssl'

KEY_ID = ENV.fetch('ASC_KEY_ID', 'H9PLG58DB9')
ISSUER = ENV.fetch('ASC_ISSUER', 'd5f5385b-5e34-4d70-925d-0debd3a3e090')
KEY_PATH = File.expand_path("~/.appstoreconnect/private_keys/AuthKey_#{KEY_ID}.p8")
APP_ID = ENV.fetch('GLIPH_APP_ID', '6805549904')
ROOT = File.expand_path('../..', __dir__)
BASE = 'https://api.appstoreconnect.apple.com'

def token
  key = OpenSSL::PKey::EC.new(File.read(KEY_PATH))
  JWT.encode({ iss: ISSUER, exp: Time.now.to_i + 900, aud: 'appstoreconnect-v1' },
             key, 'ES256', { kid: KEY_ID, typ: 'JWT' })
end

def call(method, path, body = nil)
  uri = URI("#{BASE}#{path}")
  klass = { get: Net::HTTP::Get, post: Net::HTTP::Post, patch: Net::HTTP::Patch }[method]
  req = klass.new(uri)
  req['Authorization'] = "Bearer #{token}"
  req['Content-Type'] = 'application/json'
  req.body = JSON.dump(body) if body
  res = Net::HTTP.start(uri.host, uri.port, use_ssl: true) { |h| h.request(req) }
  abort "#{method.to_s.upcase} #{path} -> #{res.code}\n#{res.body}" unless res.code.to_i.between?(200, 299)
  res.body.to_s.empty? ? {} : JSON.parse(res.body)
end

# store/metadata.md is the source: "## Name (N max)" headings, prose underneath.
def metadata
  text = File.read(File.join(ROOT, 'store', 'metadata.md'))
  section = lambda do |heading|
    m = text.match(/^## #{Regexp.escape(heading)}[^\n]*\n\n(.*?)(?=\n## |\z)/m)
    abort "store/metadata.md has no '## #{heading}' section" unless m
    m[1].strip
  end
  {
    subtitle: section.call('Subtitle'),
    promo: section.call('Promotional text').split.join(' '),
    keywords: section.call('Keywords'),
    # Everything from Description to the end is the description, subheadings
    # and all: the App Store renders it as one block of plain text.
    description: text.split(/^## Description[^\n]*\n\n/, 2)[1].to_s.strip,
  }
end

def editable_version
  vs = call(:get, "/v1/apps/#{APP_ID}/appStoreVersions?limit=10")['data']
  v = vs.find { |x| !%w[READY_FOR_SALE REPLACED_WITH_NEW_VERSION].include?(x.dig('attributes', 'appStoreState')) } || vs.first
  abort 'no app store version to edit' unless v
  v
end

case ARGV[0]
when 'state'
  v = editable_version
  puts "version #{v.dig('attributes', 'versionString')} (#{v.dig('attributes', 'appStoreState')}) #{v['id']}"
  call(:get, "/v1/appStoreVersions/#{v['id']}/appStoreVersionLocalizations")['data'].each do |l|
    a = l['attributes']
    puts "  #{a['locale']}: keywords=#{a['keywords'].inspect} " \
         "promo=#{a['promotionalText'].to_s.length} desc=#{a['description'].to_s.length} (#{l['id']})"
  end
  info = call(:get, "/v1/apps/#{APP_ID}/appInfos")['data'].first
  call(:get, "/v1/appInfos/#{info['id']}/appInfoLocalizations")['data'].each do |l|
    a = l['attributes']
    puts "  #{a['locale']}: name=#{a['name'].inspect} subtitle=#{a['subtitle'].inspect}"
  end
when 'push'
  md = metadata
  { subtitle: 30, promo: 170, keywords: 100, description: 4000 }.each do |k, max|
    abort "#{k} is #{md[k].length} chars, max #{max}" if md[k].length > max
  end

  # Keywords, description and promo text belong to the VERSION.
  v = editable_version
  call(:get, "/v1/appStoreVersions/#{v['id']}/appStoreVersionLocalizations")['data'].each do |l|
    call(:patch, "/v1/appStoreVersionLocalizations/#{l['id']}",
         { data: { type: 'appStoreVersionLocalizations', id: l['id'],
                   attributes: { keywords: md[:keywords], promotionalText: md[:promo],
                                 description: md[:description] } } })
    puts "pushed version text (#{l.dig('attributes', 'locale')})"
  end

  # The subtitle sits with the app NAME on appInfoLocalizations instead, which
  # is a different resource with a different edit state. Patching it onto the
  # version localization is a 409 with 'subtitle is not an attribute'.
  info = call(:get, "/v1/apps/#{APP_ID}/appInfos")['data'].find do |i|
    !%w[READY_FOR_SALE REPLACED_WITH_NEW_VERSION].include?(i.dig('attributes', 'appStoreState'))
  end
  abort 'no editable appInfo' unless info
  call(:get, "/v1/appInfos/#{info['id']}/appInfoLocalizations")['data'].each do |l|
    call(:patch, "/v1/appInfoLocalizations/#{l['id']}",
         { data: { type: 'appInfoLocalizations', id: l['id'],
                   attributes: { subtitle: md[:subtitle] } } })
    puts "pushed subtitle (#{l.dig('attributes', 'locale')})"
  end
else
  puts 'usage: asc-listing.rb state | push'
end
